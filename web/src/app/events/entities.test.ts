import { create, toJson } from '@bufbuild/protobuf';
import { Code, ConnectError } from '@connectrpc/connect';
import { describe, expect, it } from 'vitest';
import { AgentSchema } from '../../gen/mycel/v1/hub_pb';
import { NodeSchema, type Node } from '../../gen/mycel/v1/node_pb';
import { EventKind, EventOp, EventSchema } from '../../gen/mycel/v1/ui_pb';
import { saveSnapshot } from '../snapshot';
import { configureEntityStores, createEntityStore } from './entities';
import { agentsStore, dispatchEvent } from './stores';

const node = (name: string, online = true) => create(NodeSchema, { name, online });

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('entity stores', () => {
  it('hydrates once and replays events that arrived during the List call', async () => {
    const d = deferred<Node[]>();
    let loads = 0;
    const s = createEntityStore({
      kind: 'test-nodes-1',
      schema: NodeSchema,
      key: (n) => n.name,
      load: () => {
        loads++;
        return d.promise;
      },
    });
    const p = s.ensure();
    void s.ensure();
    s.upsert(node('b', false)); // event during load
    s.remove('a'); // deleted during load
    d.resolve([node('a'), node('b', true)]);
    await p;
    expect(loads).toBe(1);
    expect(Object.keys(s.use.getState().byId)).toEqual(['b']);
    expect(s.use.getState().byId.b.online).toBe(false);
    expect(s.use.getState().loaded).toBe(true);
  });

  it('routes stream events into the right store, deleting by event id', () => {
    agentsStore.reset();
    const a = create(AgentSchema, { id: 'agent:coder@mac', name: 'coder', nodeName: 'mac', state: 'running' });
    dispatchEvent(create(EventSchema, { kind: EventKind.AGENT, op: EventOp.CREATED, id: 'coder', body: { case: 'agent', value: a } }));
    expect(agentsStore.list().map((x) => x.id)).toEqual(['agent:coder@mac']);
    dispatchEvent(create(EventSchema, { kind: EventKind.AGENT, op: EventOp.DELETED, id: 'coder@mac' }));
    expect(agentsStore.list()).toEqual([]);
  });

  it('falls back to the last snapshot when the hub is unreachable (node mode)', async () => {
    configureEntityStores({ persist: true, scope: 'https://hub.example' });
    try {
      // A snapshot of another network under the same kind is not used.
      await saveSnapshot('entities:https://other.example:test-nodes-2', [toJson(NodeSchema, node('elsewhere'))]);
      await saveSnapshot(
        'entities:https://hub.example:test-nodes-2',
        [toJson(NodeSchema, node('remembered'))],
        new Date('2026-10-01T10:00:00Z'),
      );
      const s = createEntityStore({
        kind: 'test-nodes-2',
        schema: NodeSchema,
        key: (n) => n.name,
        load: () => Promise.reject(new ConnectError('hub unreachable', Code.Unavailable)),
      });
      await s.ensure();
      const st = s.use.getState();
      expect(st.stale).toBe(true);
      expect(st.snapshotAt).toBe('2026-10-01T10:00:00.000Z');
      expect(Object.keys(st.byId)).toEqual(['remembered']);
    } finally {
      configureEntityStores({ persist: false });
    }
  });

  it('reports other errors without a snapshot', async () => {
    const s = createEntityStore({
      kind: 'test-nodes-3',
      schema: NodeSchema,
      key: (n) => n.name,
      load: () => Promise.reject(new ConnectError('nope', Code.PermissionDenied)),
    });
    await s.ensure();
    expect(s.use.getState().error).toBe('nope');
    expect(s.use.getState().loaded).toBe(false);
  });
});
