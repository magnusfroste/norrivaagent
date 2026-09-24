# What the Norriva app must provide

The agent is deliberately dumb about Norriva. Everything it knows arrives at
login, from one page in the Norriva web app. This is that page, plus the one
endpoint that lets Norriva hand out inference with the same session.

## 1. The `/cli-auth` page

`norriva login` opens the browser at:

```
https://<norriva-app>/cli-auth?port=<PORT>&state=<STATE>
```

The page must:

1. Make sure the person is signed in (send them through your normal login if
   not, then come back here with the same query string).
2. Read `port` and `state` from the query string.
3. POST the session to the agent waiting on the laptop:

```ts
const { data: { session } } = await supabase.auth.getSession()
await fetch(`http://127.0.0.1:${port}/callback`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({
    state,                                   // echoed back; the agent refuses anything else
    access_token:  session.access_token,
    refresh_token: session.refresh_token,
    supabase_url:  import.meta.env.VITE_SUPABASE_URL,
    anon_key:      import.meta.env.VITE_SUPABASE_PUBLISHABLE_KEY,
    model: {                                 // how the agent reaches inference
      base_url: `${window.location.origin}/api/public/llm`,   // see §2
      api_key:  session.access_token,
      name:     'gpt-5.6-luna'
    }
  })
})
```

4. Show "You can close this tab" — the agent already printed "Signed in as …"
   in the terminal.

Notes:

- The POST goes to `127.0.0.1`, i.e. the person's own machine. The browser
  allows it because the agent answers the CORS preflight. Nothing leaves the
  laptop except to Norriva itself.
- `state` is a random value the agent generated for this one login. Echo it
  unchanged. A callback with the wrong state is ignored, so a stray tab cannot
  sign someone else's terminal in.
- Add `http://127.0.0.1:*` is **not** needed anywhere in Supabase Auth
  settings — the browser is already signed in; this is a hand-over, not a
  redirect.

## 2. The `llm` proxy (Norriva hands out inference)

The agent talks to an OpenAI-compatible `/chat/completions`. Norriva provides it
as a **server route in the app itself** — `/api/public/llm/$` — rather than a
Supabase edge function; on Lovable's stack that is the native place for server
code. The behaviour is what matters, and it is:

1. No `Authorization: Bearer <session>` → 401.
2. `supabase.auth.getUser(token)` fails → 401. The person, not the anon key, is
   what is verified. This check comes **before** any look at the OpenAI key, so
   a stranger with a bad token cannot learn whether the key is configured.
3. Forward `/api/public/llm/<path>` to `https://api.openai.com/v1/<path>` with
   `OPENAI_API_KEY` from the project's secrets; pass the response through
   unchanged, streaming or not.

The login page hands the agent `model.base_url = <origin>/api/public/llm` and
`model.api_key = session.access_token`, so one credential — the person's session
— covers both the data and the model, and revoking the session revokes both.

`OPENAI_API_KEY` is set once, under Project Settings → Secrets. Until it is, the
proxy answers 500 to a signed-in caller and the agent says so.

## Live

- App: https://norriva.lovable.app (published; the `id-preview--…` URL sits
  behind Lovable's own login and blocks the agent's plain HTTP calls — always
  use the published one).
- Auth: email + password, auto-confirmed for the prototype.
- Tables: `notes`, `customers` — RLS on, owner policies, comments, realtime.

## 3. Tables

Nothing special. The agent reads and writes through PostgREST as the signed-in
person, so whatever RLS policies the dashboard relies on apply to the agent
too. Give the tables `comment on table` / `comment on column` descriptions —
they show up in the OpenAPI description the agent reads with `norriva_tables`,
and a model writes better rows when the table explains itself.

For the demo, a table the dashboard shows live is the whole point:

```sql
create table public.notes (
  id          uuid primary key default gen_random_uuid(),
  owner       uuid not null default auth.uid() references auth.users(id),
  title       text not null,
  body        text,
  source      text default 'dashboard',     -- 'agent' when written from the laptop
  created_at  timestamptz not null default now()
);
alter table public.notes enable row level security;
create policy "own notes" on public.notes for all using (owner = auth.uid()) with check (owner = auth.uid());
comment on table public.notes is 'Notes a person keeps in Norriva. The agent on their laptop writes here with source = agent.';
```

Then: `norriva "read the meeting notes in this folder and add each as a note in Norriva"` — and watch the dashboard.
