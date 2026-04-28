import React, {useCallback, useEffect, useRef, useState} from 'react';
import './App.css';

// ─── Config ──────────────────────────────────────────────────────────────────
const BEARER_TOKEN = import.meta.env.VITE_BEARER_TOKEN || '';
const STREAM_ID    = import.meta.env.VITE_STREAM_ID    || 'default';
const API_BASE     = '';  // proxied via vite → http://localhost:8443

// ─── Status badges ───────────────────────────────────────────────────────────
const STATUS = {
  IDLE:        { label: 'Idle',         color: '#6b7280' },
  CONNECTING:  { label: 'Connecting…',  color: '#f59e0b' },
  CONNECTED:   { label: 'Connected',    color: '#10b981' },
  RECONNECTING:{ label: 'Reconnecting', color: '#f59e0b' },
  ERROR:       { label: 'Error',        color: '#ef4444' },
  CLOSED:      { label: 'Closed',       color: '#6b7280' },
};

// ─── Helpers ─────────────────────────────────────────────────────────────────
async function fetchTicket(bearer) {
  const res = await fetch(`${API_BASE}/v1/auth/ticket`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${bearer}` },
  });
  if (!res.ok) throw new Error(`Ticket request failed: ${res.status}`);
  const body = await res.json();
  return body.ticket;
}

// ─── Manual SSE parser ────────────────────────────────────────────────────────
// Parses a raw SSE text chunk into an array of { id, type, data } objects.
// This lets us receive ALL named event types without pre-registering them.
function parseSSEChunk(text) {
  const messages = [];
  // Each SSE message is separated by a blank line (\n\n)
  const blocks = text.split(/\n\n/);
  for (const block of blocks) {
    if (!block.trim()) continue;
    let id = '', type = 'message', data = '';
    for (const line of block.split('\n')) {
      if (line.startsWith('id:'))    id   = line.slice(3).trim();
      else if (line.startsWith('event:')) type = line.slice(6).trim();
      else if (line.startsWith('data:'))  data = line.slice(5).trim();
      else if (line.startsWith(':'))      { /* comment / heartbeat ping */ }
    }
    if (data || type) messages.push({ id, type, data });
  }
  return messages;
}

// ─── fetch-based SSE stream ───────────────────────────────────────────────────
// Returns a controller that can be used to abort the stream.
// onMessage(id, type, data), onError(err), onOpen() callbacks.
async function openSSEStream({ url, signal, onOpen, onMessage, onError }) {
  let res;
  try {
    res = await fetch(url, { signal, headers: { Accept: 'text/event-stream' } });
  } catch (e) {
    onError(e);
    return;
  }
  if (!res.ok) {
    onError(new Error(`HTTP ${res.status}`));
    return;
  }
  onOpen();
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      // Process all complete messages (ending with \n\n)
      const lastSep = buffer.lastIndexOf('\n\n');
      if (lastSep === -1) continue;
      const complete = buffer.slice(0, lastSep + 2);
      buffer = buffer.slice(lastSep + 2);
      for (const msg of parseSSEChunk(complete)) {
        onMessage(msg);
      }
    }
  } catch (e) {
    if (e.name !== 'AbortError') onError(e);
  }
}

// ─── ACK ─────────────────────────────────────────────────────────────────────
async function sendAck(bearer, { eventId, clientId, streamId }) {
  const res = await fetch(`${API_BASE}/v1/ack`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${bearer}`,
    },
    body: JSON.stringify({
      event_id:  eventId,
      client_id: clientId,
      stream_id: streamId,
      acked_at:  new Date().toISOString(),
      status:    'received',
    }),
  });
  return res;
}

function prettyData(raw) {
  try { return JSON.stringify(JSON.parse(raw), null, 2); }
  catch { return String(raw); }
}

// ─── Component ───────────────────────────────────────────────────────────────
export default function App() {
  const [token,      setToken]      = useState(BEARER_TOKEN);
  const [streamId,   setStreamId]   = useState(STREAM_ID);
  const [status,     setStatus]     = useState('IDLE');
  const [events,     setEvents]     = useState([]);
  const [heartbeats, setHeartbeats] = useState(0);
  const [log,        setLog]        = useState([]);
  const [clientId,   setClientId]   = useState('');
  const [ackStatus,  setAckStatus]  = useState({}); // eventId → 'pending'|'ok'|'err'

  const logRef = useRef(null);

  const pushLog = useCallback((msg, kind = 'info') => {
    const ts = new Date().toLocaleTimeString();
    setLog(prev => [...prev.slice(-199), { ts, msg, kind }]);
  }, []);

  // Auto-scroll log
  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [log]);

  // Derive clientId from token (JWT subject claim)
  useEffect(() => {
    if (!token) { setClientId(''); return; }
    try {
      const payload = JSON.parse(atob(token.split('.')[1]));
      setClientId(payload.sub || '');
    } catch { setClientId(''); }
  }, [token]);

  const abortRef = useRef(null);

  const connect = useCallback(async () => {
    if (!token) { pushLog('No bearer token set.', 'error'); return; }
    if (abortRef.current) abortRef.current.abort();

    setStatus('CONNECTING');
    pushLog('Fetching ticket…');

    let ticket;
    try {
      ticket = await fetchTicket(token);
      pushLog('Ticket obtained.');
    } catch (e) {
      pushLog(`Ticket error: ${e.message}`, 'error');
      setStatus('ERROR');
      return;
    }

    const controller = new AbortController();
    abortRef.current = controller;

    const url = `${API_BASE}/v1/stream/events?ticket=${encodeURIComponent(ticket)}`;

    const attemptConnect = async (attempt) => {
      if (controller.signal.aborted) return;

      if (attempt > 1) {
        setStatus('RECONNECTING');
        pushLog(`Reconnecting (attempt ${attempt})… fetching new ticket.`, 'warn');
        try {
          ticket = await fetchTicket(token);
          pushLog('New ticket obtained.');
        } catch (e) {
          pushLog(`Ticket error on reconnect: ${e.message}. Retrying in 5s…`, 'error');
          setTimeout(() => attemptConnect(attempt + 1), 10000);
          return;
        }
      }

      const connectUrl = `${API_BASE}/v1/stream/events?ticket=${encodeURIComponent(ticket)}`;

      await openSSEStream({
        url: connectUrl,
        signal: controller.signal,
        onOpen: () => {
          setStatus('CONNECTED');
          pushLog('SSE connection opened.', 'success');
        },
        onMessage: ({ id, type, data }) => {
          if (type === 'heartbeat' || (!data && !id)) {
            setHeartbeats(prev => prev + 1);
            return;
          }
          const ev = {
            id: id || crypto.randomUUID(),
            type,
            data,
            receivedAt: new Date().toISOString(),
            acked: false,
          };
          setEvents(prev => [ev, ...prev.slice(0, 499)]);
          pushLog(`Event [${ev.type}] id=${ev.id}`, 'event');
        },
        onError: (e) => {
          if (controller.signal.aborted) return;
          pushLog(`SSE error: ${e.message}. Retrying in 3s…`, 'warn');
          setTimeout(() => attemptConnect(attempt + 1), 3000);
        },
      });

      // Stream ended normally (server closed) — reconnect
      if (!controller.signal.aborted) {
        pushLog('Stream ended by server. Reconnecting in 2s…', 'warn');
        setTimeout(() => attemptConnect(attempt + 1), 2000);
      }
    };

    attemptConnect(1);
  }, [token, pushLog]);

  const disconnect = useCallback(() => {
    if (abortRef.current) {
      abortRef.current.abort();
      abortRef.current = null;
    }
    setStatus('CLOSED');
    pushLog('Disconnected.');
  }, [pushLog]);

  const ack = useCallback(async (ev) => {
    if (!clientId) { pushLog('Cannot ACK: no client_id derived from token.', 'error'); return; }
    setAckStatus(prev => ({ ...prev, [ev.id]: 'pending' }));
    pushLog(`ACKing event ${ev.id}…`);
    try {
      const res = await sendAck(token, { eventId: ev.id, clientId, streamId });
      if (res.ok || res.status === 409 /* already acked */) {
        setAckStatus(prev => ({ ...prev, [ev.id]: 'ok' }));
        setEvents(prev => prev.map(e => e.id === ev.id ? { ...e, acked: true } : e));
        pushLog(`ACK accepted for ${ev.id}.`, 'success');
      } else {
        const body = await res.json().catch(() => ({}));
        setAckStatus(prev => ({ ...prev, [ev.id]: 'err' }));
        pushLog(`ACK rejected (${res.status}): ${body.message || ''}`, 'error');
      }
    } catch (e) {
      setAckStatus(prev => ({ ...prev, [ev.id]: 'err' }));
      pushLog(`ACK error: ${e.message}`, 'error');
    }
  }, [token, clientId, streamId, pushLog]);

  const clearEvents = () => setEvents([]);

  const s = STATUS[status];

  return (
    <div className="app">
      {/* ── Header ── */}
      <header className="header">
        <div className="header-left">
          <span className="logo">🦂 Scorpion</span>
          <span className="subtitle">Event Stream Dashboard</span>
        </div>
        <div className="badge" style={{ background: s.color }}>{s.label}</div>
      </header>

      {/* ── Config panel ── */}
      <section className="config-panel">
        <div className="field">
          <label>Bearer Token</label>
          <input
            type="password"
            placeholder="eyJ…"
            value={token}
            onChange={e => setToken(e.target.value)}
            disabled={status === 'CONNECTED' || status === 'CONNECTING'}
          />
          {clientId && <span className="hint">client_id: <b>{clientId}</b></span>}
        </div>
        <div className="field">
          <label>Stream ID</label>
          <input
            type="text"
            placeholder="default"
            value={streamId}
            onChange={e => setStreamId(e.target.value)}
            disabled={status === 'CONNECTED' || status === 'CONNECTING'}
          />
        </div>
        <div className="actions">
          {(status === 'IDLE' || status === 'CLOSED' || status === 'ERROR') && (
            <button className="btn btn-primary" onClick={connect}>Connect</button>
          )}
          {(status === 'CONNECTED' || status === 'CONNECTING' || status === 'RECONNECTING') && (
            <button className="btn btn-danger" onClick={disconnect}>Disconnect</button>
          )}
          <button className="btn btn-ghost" onClick={clearEvents}>Clear Events</button>
        </div>
      </section>

      {/* ── Stats bar ── */}
      <div className="stats-bar">
        <div className="stat"><span className="stat-val">{events.length}</span><span className="stat-label">Events</span></div>
        <div className="stat"><span className="stat-val">{events.filter(e => e.acked).length}</span><span className="stat-label">ACKed</span></div>
        <div className="stat"><span className="stat-val">{events.filter(e => !e.acked).length}</span><span className="stat-label">Pending</span></div>
        <div className="stat"><span className="stat-val">{heartbeats}</span><span className="stat-label">Heartbeats</span></div>
      </div>

      <div className="main-grid">
        {/* ── Events list ── */}
        <section className="events-panel">
          <h2>Events <span className="count">{events.length}</span></h2>
          {events.length === 0 && (
            <div className="empty">No events yet. Connect to start streaming.</div>
          )}
          <ul className="event-list">
            {events.map(ev => (
              <li key={ev.id} className={`event-item ${ev.acked ? 'acked' : ''}`}>
                <div className="event-header">
                  <span className="event-type">{ev.type}</span>
                  <span className="event-id">{ev.id}</span>
                  <span className="event-time">{new Date(ev.receivedAt).toLocaleTimeString()}</span>
                </div>
                <pre className="event-data">{prettyData(ev.data)}</pre>
                <div className="event-footer">
                  {ev.acked ? (
                    <span className="ack-badge ack-ok">✓ ACKed</span>
                  ) : (
                    <button
                      className="btn btn-sm btn-ack"
                      disabled={ackStatus[ev.id] === 'pending'}
                      onClick={() => ack(ev)}
                    >
                      {ackStatus[ev.id] === 'pending' ? 'Sending…' : 'Send ACK'}
                    </button>
                  )}
                  {ackStatus[ev.id] === 'err' && (
                    <span className="ack-badge ack-err">ACK Failed</span>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </section>

        {/* ── Activity log ── */}
        <section className="log-panel">
          <h2>Activity Log</h2>
          <div className="log-scroll" ref={logRef}>
            {log.map((l, i) => (
              <div key={i} className={`log-line log-${l.kind}`}>
                <span className="log-ts">{l.ts}</span>
                <span>{l.msg}</span>
              </div>
            ))}
          </div>
        </section>
      </div>
    </div>
  );
}
