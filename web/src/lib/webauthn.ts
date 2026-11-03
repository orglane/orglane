/**
 * Passkeys (WebAuthn) in the browser. The server sends options as JSON with
 * binary values in base64url and expects credentials in the same encoding
 * (`PublicKeyCredential.toJSON()`); browsers without the native JSON helpers
 * get the conversion done here.
 */

type Json = Record<string, unknown>;

export function base64urlToBuffer(value: string): ArrayBuffer {
  const base64 = value.replace(/-/g, "+").replace(/_/g, "/");
  const padded = base64 + "=".repeat((4 - (base64.length % 4)) % 4);
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes.buffer;
}

export function bufferToBase64url(buffer: ArrayBuffer | ArrayBufferView): string {
  const bytes =
    buffer instanceof ArrayBuffer
      ? new Uint8Array(buffer)
      : new Uint8Array(buffer.buffer, buffer.byteOffset, buffer.byteLength);
  let binary = "";
  for (const b of bytes) {
    binary += String.fromCharCode(b);
  }
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Reports whether this browser can use passkeys at all. */
export function passkeysSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.PublicKeyCredential === "function" &&
    typeof navigator.credentials?.create === "function"
  );
}

interface PublicKeyCredentialStatics {
  parseCreationOptionsFromJSON?: (options: Json) => PublicKeyCredentialCreationOptions;
  parseRequestOptionsFromJSON?: (options: Json) => PublicKeyCredentialRequestOptions;
}

function statics(): PublicKeyCredentialStatics {
  return (
    typeof window.PublicKeyCredential === "function" ? window.PublicKeyCredential : {}
  ) as PublicKeyCredentialStatics;
}

function withIds(list: unknown): PublicKeyCredentialDescriptor[] | undefined {
  if (!Array.isArray(list)) {
    return undefined;
  }
  return list.map(
    (c: Json) => ({ ...c, id: base64urlToBuffer(String(c.id)) }) as PublicKeyCredentialDescriptor,
  );
}

/** Converts server creation options to what `navigator.credentials.create` takes. */
export function creationOptions(json: Json): PublicKeyCredentialCreationOptions {
  const parse = statics().parseCreationOptionsFromJSON;
  if (parse) {
    return parse(json);
  }
  const user = json.user as Json;
  const out = {
    ...json,
    challenge: base64urlToBuffer(String(json.challenge)),
    user: { ...user, id: base64urlToBuffer(String(user.id)) },
  } as unknown as PublicKeyCredentialCreationOptions;
  const exclude = withIds(json.excludeCredentials);
  if (exclude) {
    out.excludeCredentials = exclude;
  }
  return out;
}

/** Converts server request options to what `navigator.credentials.get` takes. */
export function requestOptions(json: Json): PublicKeyCredentialRequestOptions {
  const parse = statics().parseRequestOptionsFromJSON;
  if (parse) {
    return parse(json);
  }
  const out = {
    ...json,
    challenge: base64urlToBuffer(String(json.challenge)),
  } as unknown as PublicKeyCredentialRequestOptions;
  const allow = withIds(json.allowCredentials);
  if (allow) {
    out.allowCredentials = allow;
  }
  return out;
}

/** Serializes a credential the way the server expects (`toJSON()`). */
export function credentialToJSON(credential: PublicKeyCredential): Json {
  const withToJSON = credential as unknown as { toJSON?: () => Json };
  if (typeof withToJSON.toJSON === "function") {
    return withToJSON.toJSON();
  }
  const r = credential.response;
  const response: Json = { clientDataJSON: bufferToBase64url(r.clientDataJSON) };
  if ("attestationObject" in r) {
    const att = r as AuthenticatorAttestationResponse;
    response.attestationObject = bufferToBase64url(att.attestationObject);
    response.transports = typeof att.getTransports === "function" ? att.getTransports() : [];
  } else {
    const asr = r as AuthenticatorAssertionResponse;
    response.authenticatorData = bufferToBase64url(asr.authenticatorData);
    response.signature = bufferToBase64url(asr.signature);
    if (asr.userHandle) {
      response.userHandle = bufferToBase64url(asr.userHandle);
    }
  }
  return {
    id: credential.id,
    rawId: bufferToBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? undefined,
    clientExtensionResults: credential.getClientExtensionResults(),
    response,
  };
}

/**
 * Reports whether a WebAuthn failure means the person canceled (closed the
 * prompt or no passkey was picked); such failures need no error message.
 */
export function isCancellation(err: unknown): boolean {
  return (
    err instanceof DOMException && (err.name === "NotAllowedError" || err.name === "AbortError")
  );
}

/** Creates a passkey from server options and returns the serialized credential. */
export async function createPasskey(publicKey: Json): Promise<Json> {
  const credential = await navigator.credentials.create({ publicKey: creationOptions(publicKey) });
  if (!(credential instanceof PublicKeyCredential)) {
    throw new DOMException("No credential was created.", "NotAllowedError");
  }
  return credentialToJSON(credential);
}

/** Asks for a passkey assertion from server options and returns it serialized. */
export async function getPasskey(publicKey: Json): Promise<Json> {
  const credential = await navigator.credentials.get({ publicKey: requestOptions(publicKey) });
  if (!(credential instanceof PublicKeyCredential)) {
    throw new DOMException("No credential was returned.", "NotAllowedError");
  }
  return credentialToJSON(credential);
}

/** A default name for a new passkey, from the platform: "Mac", "iPhone", ... */
export function defaultPasskeyName(userAgent = navigator.userAgent): string | undefined {
  const rules: [RegExp, string][] = [
    [/iPhone/, "iPhone"],
    [/iPad/, "iPad"],
    [/Android/, "Android"],
    [/Macintosh|Mac OS X/, "Mac"],
    [/Windows/, "Windows"],
    [/CrOS/, "Chromebook"],
    [/Linux/, "Linux"],
  ];
  return rules.find(([re]) => re.test(userAgent))?.[1];
}
