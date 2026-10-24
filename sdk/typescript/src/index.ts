/**
 * Changeward SDK for TypeScript: route an agent's changes through a Changeward
 * executor (plan → policy → approval → apply → undo).
 *
 *   const cw = new Client({ url, token });
 *   const session = await cw.startSession({ target: "app", agent: "support-bot" });
 *   const change = await session.propose("UPDATE orders SET status = 'refunded' WHERE id = 42", { intent: "refund" });
 *   await change.apply();   // throws PendingApproval / Denied / Conflict
 */

export interface Plan { id: string; hash: string; diff: string; reversibility: string; blast_rows: number; warnings?: string[] }
export interface Decision { decision: "approve" | "require" | "deny"; reasons?: string[]; approvers?: string[]; channel?: "inline" | "out-of-band" }
export interface ChangeRecord { id: string; state: string; plan?: Plan; decision?: Decision; session_id?: string; receipt?: { rows_affected: number; output?: string } }
export interface SessionRecord { id: string; target: string; env: string; principal: { kind: string; agent?: string; human?: string } }

export class ChangewardError extends Error {
  readonly status: number;
  readonly body: Record<string, unknown>;
  constructor(message: string, status = 0, body: Record<string, unknown> = {}) { super(message); this.name = "ChangewardError"; this.status = status; this.body = body; }
}
/** The target changed since the plan (or since apply, for undo). Nothing was changed. */
export class Conflict extends ChangewardError {
  get conflicts(): string[] { return (this.body.conflicts as string[]) ?? []; }
}
/** Policy requires a human decision; the change was not applied. */
export class PendingApproval extends ChangewardError {
  readonly change: Change;
  constructor(change: Change) { super(`change ${change.id} needs approval: ${change.reasons.join("; ")}`); this.name = "PendingApproval"; this.change = change; }
}
/** Policy denied the change. */
export class Denied extends ChangewardError {
  readonly change: Change;
  constructor(change: Change) { super(`change ${change.id} was denied: ${change.reasons.join("; ")}`); this.name = "Denied"; this.change = change; }
}

export interface ClientOptions { url?: string; token?: string; fetch?: typeof fetch }

export class Client {
  readonly url: string;
  private readonly token: string;
  private readonly f: typeof fetch;
  constructor(opts: ClientOptions = {}) {
    const env = (globalThis as { process?: { env: Record<string, string | undefined> } }).process?.env ?? {};
    this.url = (opts.url ?? env.CHANGEWARD_EXECUTOR_URL ?? "http://127.0.0.1:8443").replace(/\/$/, "");
    this.token = opts.token ?? env.CHANGEWARD_EXECUTOR_TOKEN ?? "";
    this.f = opts.fetch ?? fetch;
  }

  /** @internal */
  async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.f(this.url + path, {
      method, headers: { authorization: `Bearer ${this.token}`, "content-type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    const data = text ? JSON.parse(text) : null;
    if (!res.ok) {
      const Err = res.status === 409 ? Conflict : ChangewardError;
      throw new Err(data?.error ?? data?.reason ?? res.statusText, res.status, data ?? {});
    }
    return data as T;
  }

  async startSession(opts: { target: string; agent?: string; incidentRef?: string; checkpoint?: boolean }): Promise<Session> {
    const s = await this.call<SessionRecord>("POST", "/v1/sessions", { target: opts.target, agent: opts.agent ?? "sdk", incident_ref: opts.incidentRef ?? "", checkpoint: !!opts.checkpoint });
    return new Session(this, s);
  }
  async session(id: string): Promise<Session> { return new Session(this, await this.call("GET", `/v1/sessions/${encodeURIComponent(id)}`)); }
  async change(ref: string): Promise<Change> { return new Change(this, await this.call("GET", `/v1/changes/${encodeURIComponent(ref)}`)); }
}

export class Session {
  readonly id: string;
  readonly client: Client;
  readonly record: SessionRecord;
  constructor(client: Client, record: SessionRecord) { this.client = client; this.record = record; this.id = record.id; }

  /** Plan a change (dry run; nothing is modified) and get the policy decision. */
  async propose(payload: string, opts: { intent?: string; kind?: string } = {}): Promise<Change> {
    return new Change(this.client, await this.client.call("POST", "/v1/changes", { session_id: this.id, payload, intent: opts.intent ?? "", kind: opts.kind ?? "", host: "sdk:typescript" }));
  }
  async query(sql: string, maxRows = 100): Promise<{ columns: string[]; rows: string[][]; truncated: boolean }> {
    return this.client.call("POST", `/v1/sessions/${encodeURIComponent(this.id)}/query`, { query: sql, max_rows: maxRows });
  }
  async changes(): Promise<Change[]> {
    const cs = await this.client.call<ChangeRecord[] | null>("GET", `/v1/sessions/${encodeURIComponent(this.id)}/changes`);
    return (cs ?? []).map((c) => new Change(this.client, c));
  }
  async end(): Promise<void> { await this.client.call("POST", `/v1/sessions/${encodeURIComponent(this.id)}/end`); }
}

export class Change {
  readonly client: Client;
  record: ChangeRecord;
  constructor(client: Client, record: ChangeRecord) { this.client = client; this.record = record; }
  get id(): string { return this.record.id; }
  get state(): string { return this.record.state; }
  get diff(): string { return this.record.plan?.diff ?? ""; }
  get reversibility(): string { return this.record.plan?.reversibility ?? ""; }
  get decision(): string { return this.record.decision?.decision ?? ""; }
  get reasons(): string[] { return this.record.decision?.reasons ?? []; }
  get needsApproval(): boolean { return this.state === "awaiting-approval"; }
  get inline(): boolean { return this.record.decision?.channel === "inline"; }

  async refresh(): Promise<this> { this.record = await this.client.call("GET", `/v1/changes/${encodeURIComponent(this.id)}`); return this; }

  /** Apply if approved; throws PendingApproval, Denied or Conflict otherwise. */
  async apply(): Promise<this> {
    await this.refresh();
    if (this.state === "rejected") throw new Denied(this);
    if (this.needsApproval) throw new PendingApproval(this);
    this.record = await this.client.call("POST", `/v1/changes/${encodeURIComponent(this.id)}/apply`);
    return this;
  }
  /** Record the delegating human's inline confirmation (only where policy allows inline). */
  async confirm(approved = true, channel = "sdk"): Promise<this> {
    this.record = await this.client.call("POST", `/v1/changes/${encodeURIComponent(this.id)}/approve-inline`, { plan_hash: this.record.plan?.hash, approved, channel });
    return this;
  }
  async undo(withDependents = false): Promise<Change[]> {
    const res = await this.client.call<{ undone?: ChangeRecord[]; dependents?: ChangeRecord[] }>("POST", `/v1/changes/${encodeURIComponent(this.id)}/undo`, { with_dependents: withDependents });
    if (res?.dependents?.length) throw new ChangewardError(`later changes depend on this one: ${res.dependents.map((d) => d.id).join(", ")}`, 0, res);
    return (res?.undone ?? []).map((c) => new Change(this.client, c));
  }
}

/** Wrap a function that returns an operation (e.g. SQL): Changeward plans it and applies it when policy auto-approves. */
export function guarded<A extends unknown[]>(session: () => Session | Promise<Session>, fn: (...args: A) => string | Promise<string>, opts: { intent?: string; apply?: boolean } = {}) {
  return async (...args: A): Promise<Change> => {
    const change = await (await session()).propose(await fn(...args), { intent: opts.intent ?? fn.name });
    if (opts.apply !== false) await change.apply();
    return change;
  };
}

/** Tool-body helper (OpenAI Agents SDK, Vercel AI SDK, Claude Agent SDK): returns model-readable text. */
export async function proposeAndApplyText(session: Session, payload: string, intent = ""): Promise<string> {
  const change = await session.propose(payload, { intent });
  try {
    await change.apply();
  } catch (e) {
    if (e instanceof PendingApproval) return `PENDING APPROVAL (${change.id}): not applied; a human must approve.\n${change.diff}\nReasons: ${change.reasons.join("; ")}. Tell the user; do not try another way.`;
    if (e instanceof Denied) return `DENIED (${change.id}): ${change.reasons.join("; ")}. Do not retry this change.`;
    if (e instanceof Conflict) return `CONFLICT: the data changed since the plan (${e.conflicts.join("; ")}). Propose the change again.`;
    throw e;
  }
  return `APPLIED ${change.id} (${change.reversibility}). Undo with change id ${change.id}.\n${change.diff}`;
}

/** LangGraph.js: pause the graph with interrupt() when approval is needed, then apply. */
export async function applyWithInterrupt(change: Change, interrupt: (value: unknown) => unknown): Promise<Change> {
  await change.refresh();
  if (change.state === "rejected") throw new Denied(change);
  if (change.needsApproval) {
    const answer = await interrupt({ changeward: { change_id: change.id, plan_id: change.record.plan?.id, diff: change.diff, reasons: change.reasons, inline: change.inline } });
    if (change.inline) {
      await change.confirm(Boolean(answer), "langgraph-interrupt");
      if (!answer) throw new Denied(change);
    }
  }
  return change.apply();
}
