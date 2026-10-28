import { useEffect, useMemo, useRef, useState, type FormEvent } from 'react';
import { Button, Dialog, Input, Select, TextArea } from '../../ds';
import { getApi } from '../../app/api';
import { useEntityList } from '../../app/events/entities';
import { channelsStore } from '../../app/events/stores';
import type { MessageInitShape } from '@bufbuild/protobuf';
import type { JobSpec, JobSpecSchema } from '../../gen/mycel/v1/job_pb';
import { errText } from '../channels/model';
import { uploadArtifact } from './artifacts';
import { useProfiles } from './JobDetail';
import { draftFrom, parseEnv, splitCommand, splitList, type JobDraft } from './model';
import s from './Jobs.module.css';

const ANY = '__any__';
const NONE = '__none__';

/** specFromDraft builds the RunJob spec from the form; returns an error string when the form is incomplete. */
export function specFromDraft(d: JobDraft): { spec: MessageInitShape<typeof JobSpecSchema> | null; error: string | null } {
  const command = splitCommand(d.command);
  if (!d.profile) return { spec: null, error: 'Pick a profile.' };
  if (command.length === 0) return { spec: null, error: 'Enter the command to run.' };
  const env = parseEnv(d.env);
  if (env.error) return { spec: null, error: env.error };
  const inputs = splitList(d.inputs).map((x) => {
    const i = x.indexOf(':');
    return i > 0 ? { artifact: x.slice(0, i), path: x.slice(i + 1) } : { artifact: x, path: '' };
  });
  if (inputs.some((i) => !i.artifact.startsWith('art_')))
    return { spec: null, error: 'Inputs are artifact ids (art_…), optionally art_…:path.' };
  let workspace: MessageInitShape<typeof JobSpecSchema>['workspace'];
  if (d.git.trim()) {
    const [url, ref = ''] = d.git.trim().split('#');
    workspace = { source: { case: 'git', value: { url, ref } } };
  }
  return {
    spec: {
      name: d.name.trim(),
      profile: d.profile,
      node: d.node,
      channel: d.channel,
      command,
      timeout: d.timeout.trim(),
      inputs,
      outputs: splitList(d.outputs),
      env: env.env,
      workspace,
    },
    error: null,
  };
}

export interface NewJobProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Prefill (run again). */
  from?: JobSpec;
  onStarted: (id: string) => void;
}

/** NewJob mirrors `mycel job run`: profile, node, channel, command, timeout, inputs, outputs, env. */
export function NewJob({ open, onOpenChange, from, onStarted }: NewJobProps) {
  const profiles = useProfiles();
  const { items: channels } = useEntityList(channelsStore);
  const [d, setD] = useState<JobDraft>(() => draftFrom(from));
  const [nodes, setNodes] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const set = <K extends keyof JobDraft>(k: K, v: JobDraft[K]) => setD((x) => ({ ...x, [k]: v }));

  useEffect(() => {
    if (open) {
      setD(draftFrom(from));
      setError(null);
    }
  }, [open, from]);

  useEffect(() => {
    if (!open || !d.profile) return;
    let live = true;
    setNodes(null);
    getApi()
      .rpc.jobs.listNodesForProfile({ profile: d.profile })
      .then((r) => live && setNodes(r.nodes))
      .catch(() => live && setNodes([]));
    return () => {
      live = false;
    };
  }, [open, d.profile]);

  const profileOptions = useMemo(
    () => (profiles ?? []).map((p) => ({ value: p.name, label: p.description ? `${p.name} · ${p.description}` : p.name })),
    [profiles],
  );
  const nodeOptions = useMemo(() => {
    const list = new Set(nodes ?? []);
    if (d.node) list.add(d.node);
    return [{ value: ANY, label: 'any node that can run it' }, ...[...list].sort().map((n) => ({ value: n, label: n }))];
  }, [nodes, d.node]);
  const channelOptions = useMemo(
    () => [{ value: NONE, label: 'none' }, ...channels.filter((c) => !c.archivedAt).map((c) => ({ value: c.name, label: c.name }))],
    [channels],
  );

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    const { spec, error: err } = specFromDraft(d);
    if (!spec) {
      setError(err);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const r = await getApi().rpc.jobs.runJob({ spec });
      onOpenChange(false);
      if (r.job) onStarted(r.job.id);
    } catch (e2) {
      setError(errText(e2));
    } finally {
      setBusy(false);
    }
  };

  const upload = async (file: File) => {
    setUploading(true);
    setError(null);
    try {
      const a = await uploadArtifact(file, { channel: d.channel || undefined });
      set('inputs', [d.inputs.trim(), a.id].filter(Boolean).join(', '));
    } catch (err) {
      setError(errText(err));
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = '';
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !busy && onOpenChange(o)}
      title={from ? 'Run again' : 'Run a job'}
      description="The same as `mycel job run`: the command runs as argv in a fresh sandbox with cwd /workspace."
      className={s.newJob}
      footer={
        <>
          <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={busy}>
            Back
          </Button>
          <Button variant="primary" type="submit" form="new-job" busy={busy}>
            Run
          </Button>
        </>
      }
    >
      <form id="new-job" className={s.form} onSubmit={(e) => void submit(e)}>
        <Input
          label="Command"
          mono
          value={d.command}
          onChange={(e) => set('command', e.target.value)}
          placeholder="python train.py --epochs 3"
          autoFocus
        />
        <div className={s.formGrid}>
          <Select
            label="Profile"
            options={profileOptions}
            value={d.profile || null}
            onValueChange={(v) => set('profile', v)}
            placeholder={profiles ? 'Profile' : 'Loading'}
          />
          <Select label="Node" options={nodeOptions} value={d.node || ANY} onValueChange={(v) => set('node', v === ANY ? '' : v)} />
          <Select
            label="Channel"
            options={channelOptions}
            value={d.channel || NONE}
            onValueChange={(v) => set('channel', v === NONE ? '' : v)}
          />
          <Input
            label="Timeout"
            mono
            value={d.timeout}
            onChange={(e) => set('timeout', e.target.value)}
            placeholder="profile limit"
            hint="Go duration, e.g. 30m"
          />
          <Input label="Name" value={d.name} onChange={(e) => set('name', e.target.value)} placeholder="optional label" />
          <Input
            label="Workspace (git)"
            mono
            value={d.git}
            onChange={(e) => set('git', e.target.value)}
            placeholder="https://…/repo.git#main"
          />
        </div>
        <div className={s.inputsRow}>
          <Input
            label="Inputs"
            mono
            value={d.inputs}
            onChange={(e) => set('inputs', e.target.value)}
            placeholder="art_…, art_…:data/train.csv"
            hint="Artifact ids, copied into /workspace/inputs"
          />
          {getApi().mode === 'hub' && (
            <>
              <input
                ref={fileRef}
                type="file"
                hidden
                onChange={(e) => e.target.files?.[0] && void upload(e.target.files[0])}
                aria-label="Upload an input file"
              />
              <Button size="sm" busy={uploading} onClick={() => fileRef.current?.click()}>
                Upload
              </Button>
            </>
          )}
        </div>
        <Input
          label="Outputs"
          mono
          value={d.outputs}
          onChange={(e) => set('outputs', e.target.value)}
          placeholder="out/*.json, model.bin"
          hint="Globs relative to /workspace"
        />
        <TextArea
          label="Environment"
          mono
          rows={3}
          value={d.env}
          onChange={(e) => set('env', e.target.value)}
          placeholder="KEY=value"
          hint={
            from && Object.keys(from.env).length
              ? 'Values of the earlier run are not shown again; fill them in.'
              : 'One KEY=value per line; MYCEL_* is reserved.'
          }
        />
        {error && (
          <p className={s.error} role="alert">
            {error}
          </p>
        )}
      </form>
    </Dialog>
  );
}
