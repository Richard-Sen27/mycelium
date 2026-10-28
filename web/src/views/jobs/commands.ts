import { toast } from '../../ds';
import { getApi } from '../../app/api';
import { registerCommands, type Command, type CommandContext } from '../../app/commands/registry';
import { jobsStore } from '../../app/events/stores';
import { isActive, jobTitle } from './model';

// Job commands for the palette (CLI: mycel job run, mycel job cancel).

const hubWrite = (ctx: CommandContext) => ctx.rights.mode === 'hub' && !ctx.rights.readOnly;

function jobCommands(ctx: CommandContext): Command[] {
  if (!hubWrite(ctx)) return [];
  const run: Command = {
    id: 'job.run',
    title: 'Run a job…',
    group: 'Jobs',
    keywords: ['job run', 'sandbox', 'execute', 'new job'],
    hint: 'mycel job run',
    run: (c) => c.navigate('/jobs?new=1'),
  };
  const cancels = jobsStore
    .list()
    .filter(isActive)
    .map((j): Command => ({
      id: `job.cancel.${j.id}`,
      title: `Cancel ${j.id}`,
      group: 'Jobs',
      keywords: ['cancel', 'stop', 'kill', jobTitle(j), j.node],
      hint: jobTitle(j).slice(0, 40),
      confirm: {
        title: `Cancel ${j.id}?`,
        body: `${jobTitle(j)} on ${j.node || 'no node yet'} stops.`,
        action: 'Cancel job',
        tone: 'alert',
      },
      run: async () => {
        await getApi().rpc.jobs.cancelJob({ id: j.id });
        toast({ title: `Cancelled ${j.id}` });
      },
    }));
  return [run, ...cancels];
}

let registered = false;

export function registerJobCommands(): void {
  if (registered) return;
  registered = true;
  registerCommands(jobCommands);
}
