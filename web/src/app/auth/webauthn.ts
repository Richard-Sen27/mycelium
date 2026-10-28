// WebAuthn glue for the hub's passkey routes (go-webauthn JSON). Uses the
// browser's JSON helpers (PublicKeyCredential.parseCreationOptionsFromJSON,
// parseRequestOptionsFromJSON, credential.toJSON) where they exist and a
// small base64url fallback elsewhere (older Safari and Firefox).

export function b64urlToBytes(s: string): Uint8Array<ArrayBuffer> {
  const b64 = s.replace(/-/g, '+').replace(/_/g, '/');
  const pad = b64.length % 4 === 0 ? '' : '='.repeat(4 - (b64.length % 4));
  const bin = atob(b64 + pad);
  const out = new Uint8Array(new ArrayBuffer(bin.length));
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function bytesToB64url(buf: ArrayBuffer | ArrayBufferView): string {
  const bytes = buf instanceof ArrayBuffer ? new Uint8Array(buf) : new Uint8Array(buf.buffer, buf.byteOffset, buf.byteLength);
  let bin = '';
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

type Json = Record<string, unknown>;

/** go-webauthn wraps options as {"publicKey": {...}}; accept both shapes. */
function unwrap(json: unknown): Json {
  const j = (json ?? {}) as Json;
  return (j.publicKey as Json) ?? j;
}

interface PKCStatic {
  parseCreationOptionsFromJSON?: (o: unknown) => PublicKeyCredentialCreationOptions;
  parseRequestOptionsFromJSON?: (o: unknown) => PublicKeyCredentialRequestOptions;
}

function pkc(): PKCStatic | undefined {
  return typeof PublicKeyCredential !== 'undefined' ? (PublicKeyCredential as unknown as PKCStatic) : undefined;
}

export function creationOptions(json: unknown): PublicKeyCredentialCreationOptions {
  const o = unwrap(json);
  const native = pkc()?.parseCreationOptionsFromJSON;
  if (native) return native(o);
  const user = o.user as Json;
  return {
    ...(o as unknown as PublicKeyCredentialCreationOptions),
    challenge: b64urlToBytes(o.challenge as string),
    user: { ...(user as unknown as PublicKeyCredentialUserEntity), id: b64urlToBytes(user.id as string) },
    excludeCredentials: ((o.excludeCredentials as Json[] | undefined) ?? []).map((c) => ({
      ...(c as unknown as PublicKeyCredentialDescriptor),
      id: b64urlToBytes(c.id as string),
    })),
  };
}

export function requestOptions(json: unknown): PublicKeyCredentialRequestOptions {
  const o = unwrap(json);
  const native = pkc()?.parseRequestOptionsFromJSON;
  if (native) return native(o);
  return {
    ...(o as unknown as PublicKeyCredentialRequestOptions),
    challenge: b64urlToBytes(o.challenge as string),
    allowCredentials: ((o.allowCredentials as Json[] | undefined) ?? []).map((c) => ({
      ...(c as unknown as PublicKeyCredentialDescriptor),
      id: b64urlToBytes(c.id as string),
    })),
  };
}

/** credentialJSON serialises a created or asserted credential for the finish routes. */
export function credentialJSON(cred: PublicKeyCredential): unknown {
  const withJSON = cred as PublicKeyCredential & { toJSON?: () => unknown };
  if (typeof withJSON.toJSON === 'function') {
    try {
      return withJSON.toJSON();
    } catch {
      /* fall through to the manual encoding */
    }
  }
  const r = cred.response;
  const response: Json = { clientDataJSON: bytesToB64url(r.clientDataJSON) };
  if ('attestationObject' in r) {
    const a = r as AuthenticatorAttestationResponse;
    response.attestationObject = bytesToB64url(a.attestationObject);
    response.transports = typeof a.getTransports === 'function' ? a.getTransports() : [];
  }
  if ('authenticatorData' in r) {
    const a = r as AuthenticatorAssertionResponse;
    response.authenticatorData = bytesToB64url(a.authenticatorData);
    response.signature = bytesToB64url(a.signature);
    response.userHandle = a.userHandle ? bytesToB64url(a.userHandle) : null;
  }
  return {
    id: cred.id,
    rawId: bytesToB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? null,
    clientExtensionResults: cred.getClientExtensionResults?.() ?? {},
    response,
  };
}

export function passkeysSupported(): boolean {
  return typeof window !== 'undefined' && typeof PublicKeyCredential !== 'undefined' && !!navigator.credentials;
}

export interface PasskeyHttp {
  passkeyRegisterBegin(name: string): Promise<unknown>;
  passkeyRegisterFinish(credential: unknown): Promise<{ id: string }>;
  passkeyLoginBegin(user?: string): Promise<unknown>;
  passkeyLoginFinish(assertion: unknown): Promise<{ user_id: string; csrf: string }>;
}

/** registerPasskey runs begin → navigator.credentials.create → finish. */
export async function registerPasskey(http: PasskeyHttp, name: string): Promise<string> {
  const begin = await http.passkeyRegisterBegin(name);
  const cred = (await navigator.credentials.create({ publicKey: creationOptions(begin) })) as PublicKeyCredential | null;
  if (!cred) throw new Error('No passkey was created.');
  const res = await http.passkeyRegisterFinish(credentialJSON(cred));
  return res.id;
}

/** loginWithPasskey runs begin → navigator.credentials.get → finish (discoverable credentials). */
export async function loginWithPasskey(http: PasskeyHttp, user?: string): Promise<{ user_id: string; csrf: string }> {
  const begin = await http.passkeyLoginBegin(user);
  const cred = (await navigator.credentials.get({ publicKey: requestOptions(begin) })) as PublicKeyCredential | null;
  if (!cred) throw new Error('No passkey was chosen.');
  return http.passkeyLoginFinish(credentialJSON(cred));
}

/** describeWebAuthnError turns DOMException names into a sentence. */
export function describeWebAuthnError(err: unknown): string {
  const name = (err as { name?: string })?.name;
  if (name === 'NotAllowedError') return 'The passkey prompt was cancelled or timed out.';
  if (name === 'InvalidStateError') return 'This authenticator already holds a passkey for this account.';
  if (name === 'SecurityError') return 'Passkeys need HTTPS and a matching hub address.';
  return (err as Error)?.message ?? String(err);
}
