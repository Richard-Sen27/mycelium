import { toast } from '../../ds';
import { getApi } from '../../app/api';
import { useFreeze } from '../../app/events/stores';

// Channel kill-switch and lifecycle calls, shared by the chat header and the
// palette commands (kept light: the commands load eagerly).

export async function freezeChannel(name: string, reason = ''): Promise<void> {
  await getApi().rpc.control.freezeChannel({ channel: name, reason: reason.trim() || 'from the UI' });
  await useFreeze.getState().load();
  toast({ title: `${name} frozen`, tone: 'alert' });
}

export async function unfreezeChannel(name: string): Promise<void> {
  await getApi().rpc.control.unfreezeChannel({ channel: name });
  await useFreeze.getState().load();
  toast({ title: `${name} unfrozen` });
}

export async function archiveChannel(name: string): Promise<void> {
  await getApi().rpc.channels.archiveChannel({ name });
  toast({ title: `${name} archived` });
}
