import { useMemo, useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import { Badge, Checkbox, cx, EmptyState, Frame, Input, List, Skeleton } from '../../ds';
import { useEntityList } from '../../app/events/entities';
import { channelsStore, useFreeze } from '../../app/events/stores';
import { useRights } from '../../app/session';
import type { Channel } from '../../gen/mycel/v1/channel_pb';
import { BudgetGauge, hasBudget } from './Budget';
import { Chat } from './Chat';
import { CHANNEL_KIND, channelPath, isChannelFrozen, readCursor, useReads } from './model';
import { useChannelUsage } from './usage';
import s from './Channels.module.css';

const byName = (a: Channel, b: Channel) => Number(!!a.archivedAt) - Number(!!b.archivedAt) || a.name.localeCompare(b.name);

/** Channels: the channel list beside one channel's chat (`/channels/<url-encoded name>`). */
export function Component() {
  const params = useParams();
  // React Router has already decoded the splat ("%23backend" → "#backend").
  const name = params['*'] ?? '';
  return (
    <div className={s.layout} data-open={name ? true : undefined}>
      <ChannelList selected={name} />
      {name ? (
        <Chat key={name} name={name} />
      ) : (
        <Frame title="Chat" className={cx(s.chatFrame, s.noChannel)}>
          <EmptyState title="Pick a channel">Messages, threads and the blackboard of the channel show here.</EmptyState>
        </Frame>
      )}
    </div>
  );
}

function ChannelList({ selected }: { selected: string }) {
  const navigate = useNavigate();
  const rights = useRights();
  const { items, loaded, error, stale } = useEntityList(channelsStore, byName);
  const freeze = useFreeze((st) => st.status);
  useReads((st) => st.v);
  const [q, setQ] = useState('');
  const [archived, setArchived] = useState(false);
  const rows = useMemo(
    () => items.filter((c) => (archived || !c.archivedAt) && (!q || c.name.toLowerCase().includes(q.toLowerCase()))),
    [items, q, archived],
  );
  return (
    <Frame
      title="Channels"
      index="02"
      flush
      className={s.listFrame}
      bodyClassName={s.listBody}
      meta={
        <>
          {stale && <Badge tone="off">stale</Badge>}
          <span>{rows.length}</span>
        </>
      }
    >
      <div className={s.listTools}>
        <Input aria-label="Filter channels" placeholder="Filter" mono value={q} onChange={(e) => setQ(e.target.value)} />
        <Checkbox label="archived" checked={archived} onCheckedChange={setArchived} />
      </div>
      {!loaded ? (
        error ? (
          <EmptyState title="Could not load channels">{error}</EmptyState>
        ) : (
          <Skeleton rows={6} />
        )
      ) : (
        <List
          aria-label="Channels"
          items={rows}
          itemKey={(c) => c.name}
          selectedKey={selected || null}
          onActivate={(c) => navigate(channelPath(c.name))}
          empty={
            <EmptyState title={q ? 'No channel matches' : 'No channels'}>
              {q
                ? 'Try a shorter filter.'
                : rights.mode === 'hub'
                  ? 'Create one with `mycel channel create`.'
                  : 'This node sees no channels.'}
            </EmptyState>
          }
          renderItem={(c) => <ChannelRow c={c} frozen={isChannelFrozen(c.name, freeze)} />}
        />
      )}
    </Frame>
  );
}

function ChannelRow({ c, frozen }: { c: Channel; frozen: boolean }) {
  const budget = hasBudget(c.policy?.budget);
  const usage = useChannelUsage(c.name, budget);
  const unread = !c.archivedAt && c.lastSeq > 0n && c.lastSeq > readCursor(c.name);
  return (
    <div className={s.chRow} data-unread={unread || undefined}>
      <div className={s.chLine}>
        <span className={s.unreadDot} aria-hidden />
        <span className={s.chName}>{c.name}</span>
        {unread && <span className={s.visuallyHidden}>unread</span>}
        {frozen && (
          <Badge tone="alert" pulse>
            frozen
          </Badge>
        )}
        {c.archivedAt && <Badge tone="off">archived</Badge>}
      </div>
      <div className={s.chMeta}>
        <span>{CHANNEL_KIND[c.type]}</span>
        <span>{c.members.length} members</span>
      </div>
      {budget && (
        <div className={s.chBudget}>
          <BudgetGauge budget={c.policy?.budget} used={usage?.used} compact />
        </div>
      )}
    </div>
  );
}
