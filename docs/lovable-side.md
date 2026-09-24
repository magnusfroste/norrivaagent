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
    model: {                                 // optional — see §2
      base_url: `${import.meta.env.VITE_SUPABASE_URL}/functions/v1/llm`,
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

## 2. The `llm` edge function (Norriva hands out inference)

The agent talks to an OpenAI-compatible `/chat/completions`. The simplest way
for Norriva to hand out inference at login is a tiny proxy edge function that:

1. Verifies the `Authorization: Bearer <supabase access token>` header with
   `supabase.auth.getUser()`. No user, no inference.
2. Forwards the request body to OpenAI with Norriva's key.
3. Streams or returns the response unchanged.

```ts
// supabase/functions/llm/index.ts
Deno.serve(async (req) => {
  const jwt = req.headers.get('authorization')?.replace('Bearer ', '')
  const { data: { user } } = await supabase.auth.getUser(jwt)
  if (!user) return new Response('sign in first', { status: 401 })

  const url = new URL(req.url)                       // …/functions/v1/llm/chat/completions
  const upstream = 'https://api.openai.com/v1' + url.pathname.replace(/^.*\/llm/, '')
  return fetch(upstream, {
    method: req.method,
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${Deno.env.get('OPENAI_API_KEY')}` },
    body: req.body,
  })
})
```

With this in place, one credential — the person's session — covers both the
data and the model, and revoking the session revokes both. Until it exists,
the agent runs against a plain key:

```
NORRIVA_MODEL_URL=https://api.openai.com/v1 NORRIVA_MODEL_KEY=sk-… norriva "…"
```

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
