import { fromJson, type JsonValue } from '@bufbuild/protobuf';
import { useState } from 'react';
import { toast } from '../../ds';
import { getApi, type Api } from '../../app/api';
import { ArtifactSchema, type Artifact } from '../../gen/mycel/v1/job_pb';
import s from './Jobs.module.css';

const ARTIFACT_ID = /^art_[0-9A-Za-z]+$/;

/** isArtifactId: only well-formed ids become links. */
export function isArtifactId(id: string): boolean {
  return ARTIFACT_ID.test(id);
}

/** artifactPath is the hub route for an artifact's bytes. */
export function artifactPath(id: string): string {
  return `/api/artifacts/${encodeURIComponent(id)}`;
}

/**
 * artifactUrl is where a link points: the hub route itself (the session
 * cookie authenticates a plain GET), or the same route through the node
 * daemon's read-only proxy in node mode.
 */
export function artifactUrl(mode: Api['mode'], id: string): string {
  return mode === 'node' ? `/api/hub${artifactPath(id)}` : artifactPath(id);
}

/**
 * ArtifactLink downloads an artifact. In hub mode it is a plain link (the hub
 * sends it as an attachment). In node mode the proxy needs the local bearer,
 * which a link cannot carry, so the click goes through getApi().download().
 * A malformed id is shown as text, never as a link.
 */
export function ArtifactLink({ id, name, className }: { id: string; name?: string; className?: string }) {
  const api = getApi();
  const [busy, setBusy] = useState(false);
  const label = name || id;
  if (!isArtifactId(id)) return <span className={className}>{label}</span>;
  const href = artifactUrl(api.mode, id);
  if (api.mode === 'hub')
    return (
      <a className={className ?? s.link} href={href} download={name || undefined}>
        {label}
      </a>
    );
  const save = async () => {
    setBusy(true);
    try {
      const url = URL.createObjectURL(await api.download(artifactPath(id)));
      const a = document.createElement('a');
      a.href = url;
      a.download = name || id;
      a.click();
      setTimeout(() => URL.revokeObjectURL(url), 10_000);
    } catch (err) {
      toast({ title: 'Download failed', body: err instanceof Error ? err.message : String(err), tone: 'alert' });
    } finally {
      setBusy(false);
    }
  };
  return (
    <a
      className={className ?? s.link}
      href={href}
      aria-busy={busy || undefined}
      onClick={(e) => {
        e.preventDefault();
        if (!busy) void save();
      }}
    >
      {label}
    </a>
  );
}

/** uploadArtifact stores a file on the hub (PUT /api/artifacts?name=…) for a job's inputs; hub mode only. */
export async function uploadArtifact(file: File, opts: { channel?: string } = {}): Promise<Artifact> {
  const q = new URLSearchParams({ name: file.name });
  if (opts.channel) q.set('channel', opts.channel);
  const j = await getApi().upload(`/api/artifacts?${q}`, file);
  return fromJson(ArtifactSchema, j as JsonValue, { ignoreUnknownFields: true });
}
