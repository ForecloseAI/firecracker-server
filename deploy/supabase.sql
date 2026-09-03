-- Run once in the Supabase SQL editor before setting COMPOSIO_API_KEY.
-- The chat service accesses this table with the caller's JWT, never a service key.
create table if not exists public.app_sessions (
  user_id uuid primary key references auth.users(id) on delete cascade,
  session_id text not null,
  mcp_url text not null,
  updated_at timestamptz not null default now()
);

alter table public.app_sessions enable row level security;

drop policy if exists "users manage their own app session" on public.app_sessions;
create policy "users manage their own app session"
  on public.app_sessions
  for all
  to authenticated
  using ((select auth.uid()) = user_id)
  with check ((select auth.uid()) = user_id);

grant select, insert, update, delete on public.app_sessions to authenticated;

-- Added 2026-09-03 for per-capability app permissions. Idempotent; run it in the
-- SQL editor before deploying the release that serves PUT /v1/apps/{slug}/policy.
--
-- Nullable rather than `not null default '{}'`: absent and empty mean the same
-- thing to the reader -- ask about everything -- and a null round-trips to a nil
-- map, so the client is never handed an empty object it has to special-case.
--
-- No new policy or grant. The row-level policy above is `for all` on the table
-- and the grant is table-wide, so a new column is covered by both. Which is also
-- the thing to remember about this table: the person reaches it with their own
-- token, so what is stored here is their PREFERENCE and not a limit imposed on
-- them.
alter table public.app_sessions
  add column if not exists policy jsonb;
