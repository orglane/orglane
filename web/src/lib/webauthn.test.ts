import { describe, expect, it } from "vitest";

import {
  base64urlToBuffer,
  bufferToBase64url,
  creationOptions,
  defaultPasskeyName,
  requestOptions,
} from "./webauthn";

describe("base64url", () => {
  it("round-trips binary data without padding", () => {
    const bytes = new Uint8Array([0, 1, 2, 250, 251, 252, 253, 254, 255]);
    const encoded = bufferToBase64url(bytes);
    expect(encoded).not.toMatch(/[+/=]/);
    expect(new Uint8Array(base64urlToBuffer(encoded))).toEqual(bytes);
  });
});

describe("options from JSON (browsers without the native parsers)", () => {
  it("decodes creation options", () => {
    const opts = creationOptions({
      challenge: "AAEC",
      rp: { id: "work.acme.com", name: "Orglane" },
      user: { id: "AQID", name: "alex@acme.com", displayName: "Alex" },
      pubKeyCredParams: [{ type: "public-key", alg: -7 }],
      excludeCredentials: [{ type: "public-key", id: "BAUG" }],
    });
    expect(new Uint8Array(opts.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
    expect(new Uint8Array(opts.user.id as ArrayBuffer)).toEqual(new Uint8Array([1, 2, 3]));
    expect(new Uint8Array(opts.excludeCredentials?.[0]?.id as ArrayBuffer)).toEqual(
      new Uint8Array([4, 5, 6]),
    );
    expect(opts.rp.id).toBe("work.acme.com");
  });

  it("decodes request options", () => {
    const opts = requestOptions({
      challenge: "AAEC",
      rpId: "work.acme.com",
      userVerification: "preferred",
    });
    expect(new Uint8Array(opts.challenge as ArrayBuffer)).toEqual(new Uint8Array([0, 1, 2]));
    expect(opts.allowCredentials).toBeUndefined();
  });
});

describe("defaultPasskeyName", () => {
  it("names the platform", () => {
    expect(defaultPasskeyName("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")).toBe("Mac");
    expect(defaultPasskeyName("Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)")).toBe(
      "iPhone",
    );
    expect(defaultPasskeyName("Mozilla/5.0 (Linux; Android 15)")).toBe("Android");
    expect(defaultPasskeyName("curl/8")).toBeUndefined();
  });
});
