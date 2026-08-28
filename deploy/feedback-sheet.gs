/**
 * Appends one task-feedback row to the spreadsheet this script is bound to.
 *
 * Deploying it:
 *   1. Open the target Google Sheet, then Extensions > Apps Script.
 *   2. Replace Code.gs with this file and save.
 *   3. Deploy > New deployment > type "Web app".
 *      Execute as:     Me
 *      Who has access: Anyone        <-- NOT "Anyone with a Google account".
 *   4. Copy the /exec URL and give it to the chat service:
 *        sudo systemctl edit cracked-chat
 *        Environment=FEEDBACK_WEBHOOK_URL=https://script.google.com/macros/s/<id>/exec
 *      It is a bearer credential -- anyone holding it can append rows -- so it
 *      belongs in the drop-in, never in the checked-in .service file.
 *
 * If posting to it returns 401 with a Drive "unable to open the file" page, the
 * deployment is Workspace-scoped: the URL will contain /a/macros/<domain>/ and
 * "Who has access" is still restricted. Editing the deployment keeps the URL.
 */

// Column order. It must match feedbackRow in internal/chat/feedback.go: the
// server sends an object, and this is the only thing that decides where each
// value lands.
const HEADERS = [
  'time', 'email', 'machine', 'agentId', 'taskTitle',
  'taskSlug', 'rating', 'comment', 'platform', 'appVersion',
];

/**
 * Receives one row from the chat service.
 *
 * The reply is always HTTP 200 -- Apps Script cannot set a status code -- so
 * the caller reads `ok` from the body to tell a stored row from a thrown one.
 */
function doPost(e) {
  const lock = LockService.getScriptLock();
  try {
    // Two ratings submitted at once would otherwise read the same last row and
    // one would overwrite the other.
    lock.waitLock(20000);
    appendRow_(JSON.parse(e.postData.contents));
    return json_({ ok: true });
  } catch (err) {
    return json_({ ok: false, error: String(err) });
  } finally {
    lock.releaseLock();
  }
}

/** Writes one row, adding the header line to a fresh sheet. */
function appendRow_(row) {
  const sheet = SpreadsheetApp.getActiveSpreadsheet().getSheets()[0];
  if (sheet.getLastRow() === 0) {
    sheet.appendRow(HEADERS);
    sheet.setFrozenRows(1);
  }
  sheet.appendRow(HEADERS.map((h) => literal_(row[h])));
}

/**
 * Keeps what a person typed from being read as a formula.
 *
 * Sheets parses a cell starting with =, +, - or @ as a formula, and appendRow
 * writes with the same semantics as typing -- so a comment of "=IMPORTXML(...)"
 * would run under the sheet owner's account instead of being feedback, and a
 * task title starting with "=" would land as #NAME? rather than as itself. A
 * leading apostrophe pins the cell to literal text and is consumed, not stored.
 *
 * Numbers pass through untouched, so the rating column still adds up.
 */
function literal_(v) {
  if (v === undefined || v === null) return '';
  if (typeof v !== 'string') return v;
  return /^[=+\-@\t\r]/.test(v) ? "'" + v : v;
}

/** Answers with JSON rather than Apps Script's HTML error wrapper. */
function json_(body) {
  return ContentService.createTextOutput(JSON.stringify(body))
    .setMimeType(ContentService.MimeType.JSON);
}
