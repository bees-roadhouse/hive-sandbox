// What the daemon's JSON looks like on the wire. Field names are the API's,
// not ours: the page is a view of the daemon, not a model of its own.

export interface Me {
  version: string;
  actor: { id: string; kind: string; handle: string; display_name: string };
  principal: { kind: string; id: string };
  credential: { id: string; label: string; created_at: string; last_used_at: string | null };
}

export interface Conversation {
  id: string;
  runtime: string;
  model: string;
  title: string;
  author_actor: string;
  owner: { kind: string; id: string };
  created_at: string;
  updated_at: string;
}

export interface Message {
  seq: number;
  role: string;
  author_actor: string;
  body: string;
  trust: string;
  run_id?: string;
  created_at: string;
}

export interface Turn {
  request_seq: number;
  state: string;
}

/** One frame off the conversation stream: a line the agent's run produced. */
export interface RunFrame {
  request_seq: number;
  seq: number;
  stream: string;
  type: string;
  text?: string;
}
