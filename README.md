# norriva

**The agent on your own computer, signed in to Norriva.**

Norriva is point-and-click: a dashboard, tables, a team. This is the other half
— a command on your laptop that logs in to the same Norriva, is allowed into a
folder you choose, and works with the shared data as you.

```sh
curl -fsSL https://norriva.example/install.sh | sh

norriva login                     # opens the browser; you sign in to Norriva
norriva link ./customer-project   # the folder the agent may work in
norriva "read the notes in this folder and add each customer to Norriva"
```

Then look at the dashboard. The rows are there.

## Why it is built this way

**The agent is you.** After `norriva login` the agent holds your Norriva
session and nothing else. Every read and write goes through Norriva's own API
with that session, so the row-level security your dashboard already relies on
applies to the agent too. It can do exactly what you can, and not one row more.

**Login hands out everything.** The browser page you sign in on gives the
agent your session, where the data lives, and how to reach a model — one
credential for both. Revoke the session and the agent is blind.

**The folder is a fence.** `norriva link` names a folder; the file tools refuse
any path that resolves outside it. Nothing on your disk is reachable by
accident.

**Bring your own agent.** `norriva mcp` serves the same tools over MCP, so
opencode, Claude Code, Kilo or Hermes can reach your Norriva data as you. The
built-in loop is there for a demo and for people who do not run an agent yet;
the tools are the product.

## Commands

| | |
|---|---|
| `norriva login [--host URL]` | sign in to Norriva in the browser |
| `norriva status` | who you are, what is linked, which model |
| `norriva link <path> [--name N]` | let the agent work in a folder |
| `norriva "<instruction>"` | do one thing and report |
| `norriva chat` | a conversation |
| `norriva mcp` | the tools over MCP, for another agent |
| `norriva logout` | forget the session; keep the folders |

## For another agent

```json
{ "mcpServers": { "norriva": { "command": "norriva", "args": ["mcp"] } } }
```

Tools: `list_files`, `read_file`, `write_file` (inside the linked folder);
`norriva_tables`, `norriva_query`, `norriva_insert`, `norriva_update` (as you).

## Building

Go, standard library only.

```sh
make            # ./dist/norriva-{darwin,linux}-{arm64,amd64}
```

See [docs/lovable-side.md](docs/lovable-side.md) for the one page and one
endpoint the Norriva app provides.
