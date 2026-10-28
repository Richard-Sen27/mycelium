import { useSearchParams } from 'react-router';
import { Tabs } from '../../ds';
import { AuditExplorer } from './AuditExplorer';
import { Cost } from './Cost';
import { Replay } from './Replay';
import s from './Traces.module.css';

type TraceTab = 'cost' | 'replay' | 'audit';
const TABS: TraceTab[] = ['cost', 'replay', 'audit'];

/** Traces and cost: spend per agent, channel and grant; thread replay by trace id; the audit explorer. */
export function Component() {
  const [params, setParams] = useSearchParams();
  const trace = params.get('trace') ?? '';
  const t = params.get('tab') as TraceTab | null;
  const tab: TraceTab = t && TABS.includes(t) ? t : trace ? 'replay' : 'cost';

  const go = (next: { tab?: TraceTab; trace?: string }) => {
    const p = new URLSearchParams(params);
    if (next.tab) p.set('tab', next.tab);
    if (next.trace !== undefined) {
      if (next.trace) p.set('trace', next.trace);
      else p.delete('trace');
    }
    setParams(p, { replace: true });
  };

  return (
    <section className={s.page} aria-labelledby="traces-heading">
      <h1 className={s.heading} id="traces-heading">
        Traces and cost
      </h1>
      <Tabs<TraceTab>
        aria-label="Traces"
        value={tab}
        onValueChange={(v) => go({ tab: v })}
        className={s.tabs}
        items={[
          { value: 'cost', label: 'Cost', panel: <Cost /> },
          { value: 'replay', label: 'Replay', panel: <Replay initial={trace} onTrace={(tr) => go({ tab: 'replay', trace: tr })} /> },
          { value: 'audit', label: 'Audit', panel: <AuditExplorer onTrace={(tr) => go({ tab: 'replay', trace: tr })} /> },
        ]}
      />
    </section>
  );
}
