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
