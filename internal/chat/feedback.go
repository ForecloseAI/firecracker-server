package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// feedbackReq is what the app sends after a task ends.
//
// Nothing identifying is taken from it. The person and the time are stamped
// server-side from the bearer token, because a client that could name someone
// else in a feedback row would be a way to put words in their mouth.
type feedbackReq struct {
	AgentID    string `json:"agentId"`
	Rating     int    `json:"rating"`
	Comment    string `json:"comment"`
	TaskTitle  string `json:"taskTitle"`
	TaskSlug   string `json:"taskSlug"`
	Platform   string `json:"platform"`
	AppVersion string `json:"appVersion"`
}

// feedbackRow is one line in the sheet. These json tags must match HEADERS in
// deploy/feedback-sheet.gs, which maps by name rather than position -- so a
// renamed tag does not error, it writes into no column at all, silently and
// for as long as nobody looks.
type feedbackRow struct {
	Time       string `json:"time"`
	Email      string `json:"email"`
	Machine    string `json:"machine"`
	AgentID    string `json:"agentId"`
	TaskTitle  string `json:"taskTitle"`
	TaskSlug   string `json:"taskSlug"`
	Rating     int    `json:"rating"`
	Comment    string `json:"comment"`
	Platform   string `json:"platform"`
	AppVersion string `json:"appVersion"`
}

// postFeedback records how a person rated a finished task.
//
// It never reaches the guest, and that is load-bearing twice over: the agent
// must not learn it was rated, and guestOf BOOTS A MACHINE on demand -- so
// touching it here would spend a minute and a VM on a star rating.
func (s *Server) postFeedback(w http.ResponseWriter, r *http.Request, user string) {
	var req feedbackReq
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	if req.AgentID == "" || req.Rating < 1 || req.Rating > 5 {
		fail(w, http.StatusBadRequest, "an agentId and a rating from 1 to 5 are required")
		return
	}
	if s.feedback == nil {
		log.Printf("chat: feedback from %s dropped: FEEDBACK_WEBHOOK_URL is not set", user)
		fail(w, http.StatusServiceUnavailable, "feedback is not configured")
		return
	}
	s.forwardFeedback(w, user, req)
}

// forwardFeedback stamps the row and sends it, mapping a webhook failure.
func (s *Server) forwardFeedback(w http.ResponseWriter, user string, req feedbackReq) {
	row := feedbackRow{
		Time: time.Now().UTC().Format(time.RFC3339), Email: user,
		Machine: machineFor(user), AgentID: req.AgentID,
		TaskTitle: req.TaskTitle, TaskSlug: req.TaskSlug,
		Rating: req.Rating, Comment: req.Comment,
		Platform: req.Platform, AppVersion: req.AppVersion,
	}
	if err := s.feedback.send(row); err != nil {
		log.Printf("chat: feedback from %s not recorded: %v", user, err)
		fail(w, http.StatusBadGateway, "could not record feedback")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sheet posts rows to a Google Apps Script web app bound to a spreadsheet.
//
// A webhook rather than the Sheets API on purpose: it needs no service account,
// no key on the host and no new dependency, and the whole contract is one URL.
type sheet struct {
	url  string
	http *http.Client
}

// newSheet builds the client, or nil when no webhook is configured. Nil is the
// off switch: an unset URL leaves the endpoint answering 503 rather than
// pretending to have stored something.
func newSheet(url string) *sheet {
	if url == "" {
		return nil
	}
	// Short, because the app is waiting on this to say thank you.
	return &sheet{url: url, http: &http.Client{Timeout: 5 * time.Second}}
}

// send delivers one row.
//
// A successful doPost answers 302 to script.googleusercontent.com carrying the
// real body; Go turns a 302 into a GET and follows it, so what arrives here is
// already the script's own reply. A 401 or an HTML sign-in page means the
// deployment's "Who has access" is not "Anyone", not that the row was bad.
func (s *sheet) send(row feedbackRow) error {
	buf, err := json.Marshal(row)
	if err != nil {
		return err
	}
	res, err := s.http.Post(s.url, "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("sheet webhook answered %d", res.StatusCode)
	}
	return sheetVerdict(res.Body)
}

// sheetVerdict reads the script's own answer.
//
// Apps Script cannot set a status code: everything it manages to send comes
// back 200. So a script that threw on a bad row looks exactly like one that
// stored it, unless the body is read and believed over the status.
func sheetVerdict(r io.Reader) error {
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 4<<10)).Decode(&out); err != nil {
		return fmt.Errorf("sheet webhook sent no verdict: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("sheet webhook refused the row: %s", out.Error)
	}
	return nil
}
