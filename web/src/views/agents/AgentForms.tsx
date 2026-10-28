import { useMemo, useState, type FormEvent } from 'react';
import { Button, EmptyState, Input, Select, TextArea } from '../../ds';
import { ChannelType } from '../../gen/mycel/v1/channel_pb';
import { getApi } from '../../app/api';
import { useEntity, useEntityList } from '../../app/events/entities';
import { agentsStore, channelsStore } from '../../app/events/stores';
import { act, errText } from '../nodes/shared';
import s from './page.module.css';
import { inspectAgent } from './AgentDetail';
import { HARNESSES, buildSpawnPost } from './agents';

function Back({ id }: { id: string }) {
  return (
    <Button variant="ghost" onClick={() => inspectAgent(id)}>
      Back
    </Button>
  );
}

function useSubmit(id: string) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const run = async (fn: () => Promise<unknown>, ok: string) => {
    setBusy(true);
    setError(null);
    try {
      await act(fn, ok);
      inspectAgent(id);
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  };
  return { busy, error, setError, run };
}

/** PauseForm: kill-switch level one with a reason (`mycel agent pause`). */
export function PauseForm({ id }: { id: string }) {
  const agent = useEntity(agentsStore, id);
  const [reason, setReason] = useState('');
  const { busy, error, run } = useSubmit(id);
  if (!agent) return <EmptyState title="This agent is gone" />;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void run(() => getApi().rpc.agents.pauseAgent({ id, reason: reason.trim() }), `Paused ${agent.name}`);
  };
  return (
    <form className={s.form} onSubmit={submit} aria-label={`Pause ${agent.name}`}>
      <p className={s.note}>The agent stops taking turns until it is resumed. Its sandbox and grants stay.</p>
      <Input label="Reason" value={reason} onChange={(e) => setReason(e.target.value)} placeholder="operator" maxLength={200} autoFocus />
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Back id={id} />
        <Button type="submit" variant="primary" busy={busy}>
          Pause agent
        </Button>
      </div>
    </form>
  );
}

const NUM = /^\d+$/;
const USD = /^\d+(\.\d{1,4})?$/;

/** BudgetForm sets the per-agent ceiling (`mycel agent budget`); empty = unlimited. */
export function BudgetForm({ id }: { id: string }) {
  const agent = useEntity(agentsStore, id);
  const [tokens, setTokens] = useState(() => (agent?.budget?.tokens ? String(agent.budget.tokens) : ''));
  const [usd, setUsd] = useState(() => (agent?.budget?.usd ? String(agent.budget.usd) : ''));
  const { busy, error, run } = useSubmit(id);
  if (!agent) return <EmptyState title="This agent is gone" />;
  const tokErr = tokens && !NUM.test(tokens) ? 'Whole tokens.' : undefined;
  const usdErr = usd && !USD.test(usd) ? 'Dollars, e.g. 2.50.' : undefined;
  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (tokErr || usdErr) return;
    void run(
      () => getApi().rpc.agents.setAgentBudget({ id, budget: { tokens: BigInt(tokens || '0'), usd: Number(usd || '0') } }),
      `Budget set for ${agent.name}`,
    );
  };
  return (
    <form className={s.form} onSubmit={submit} aria-label={`Budget of ${agent.name}`}>
      <p className={s.note}>When either limit is reached the hub pauses the agent. Leave a field empty for no limit.</p>
      <div className={s.formRow}>
        <Input
          label="Tokens"
          mono
          inputMode="numeric"
          value={tokens}
          onChange={(e) => setTokens(e.target.value.trim())}
          error={tokErr}
          placeholder="unlimited"
        />
        <Input
          label="USD"
          mono
          inputMode="decimal"
          value={usd}
          onChange={(e) => setUsd(e.target.value.trim())}
          error={usdErr}
          placeholder="unlimited"
        />
      </div>
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Back id={id} />
        <Button type="submit" variant="primary" busy={busy}>
          Set budget
        </Button>
      </div>
    </form>
  );
}

/** SpawnForm asks an orchestrator to start a worker on its node (see buildSpawnPost). */
export function SpawnForm({ id }: { id: string }) {
  const agent = useEntity(agentsStore, id);
  const channels = useEntityList(channelsStore).items;
  const options = useMemo(
    () =>
      channels
        .filter((c) => !c.archivedAt && c.type !== ChannelType.DIRECT)
        .map((c) => ({ value: c.name, label: c.name }))
        .sort((a, b) => a.value.localeCompare(b.value)),
    [channels],
  );
  const [harness, setHarness] = useState<string>(HARNESSES[0]);
  const [task, setTask] = useState('');
  const [channel, setChannel] = useState<string | null>(agent?.homeChannel || null);
  const [budgetTokens, setBudgetTokens] = useState('');
  const [budgetUsd, setBudgetUsd] = useState('');
  const [tried, setTried] = useState(false);
  const { busy, error, run } = useSubmit(id);
  if (!agent) return <EmptyState title="This agent is gone" />;

  const errs = {
    task: !task.trim() ? 'Describe the task.' : undefined,
    channel: !channel ? 'Pick the channel the worker reports to.' : undefined,
    tokens: budgetTokens && !NUM.test(budgetTokens) ? 'Whole tokens.' : undefined,
    usd: budgetUsd && !USD.test(budgetUsd) ? 'Dollars.' : undefined,
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setTried(true);
    if (Object.values(errs).some(Boolean) || !channel) return;
    const req = buildSpawnPost(agent, { harness, task, channel, budgetTokens, budgetUsd });
    void run(() => getApi().rpc.channels.post(req), `Asked ${agent.name} to spawn a ${harness} worker`);
  };
  return (
    <form className={s.form} onSubmit={submit} aria-label={`Spawn a worker from ${agent.name}`}>
      <p className={s.note}>
        Workers start on {agent.nodeName}. The request goes to {agent.name} as a task message; it calls spawn_agent with these arguments.
      </p>
      <Select label="Harness" value={harness} onValueChange={setHarness} options={HARNESSES.map((h) => ({ value: h, label: h }))} />
      <TextArea label="Task" rows={4} value={task} onChange={(e) => setTask(e.target.value)} error={tried ? errs.task : undefined} />
      {options.length > 0 ? (
        <Select label="Channel" value={channel} onValueChange={setChannel} options={options} placeholder="Pick a channel" />
      ) : (
        <Input label="Channel" mono value={channel ?? ''} onChange={(e) => setChannel(e.target.value)} placeholder="#channel" />
      )}
      {tried && errs.channel && <p className={s.formError}>{errs.channel}</p>}
      <div className={s.formRow}>
        <Input
          label="Budget tokens"
          mono
          inputMode="numeric"
          value={budgetTokens}
          onChange={(e) => setBudgetTokens(e.target.value.trim())}
          error={errs.tokens}
          placeholder="none"
        />
        <Input
          label="Budget USD"
          mono
          inputMode="decimal"
          value={budgetUsd}
          onChange={(e) => setBudgetUsd(e.target.value.trim())}
          error={errs.usd}
          placeholder="none"
        />
      </div>
      {error && <p className={s.formError}>{error}</p>}
      <div className={s.formActions}>
        <Back id={id} />
        <Button type="submit" variant="primary" busy={busy}>
          Send request
        </Button>
      </div>
    </form>
  );
}
