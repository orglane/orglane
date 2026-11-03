import { useMutation, useQueryClient } from "@tanstack/react-query";

import { call, meKey } from "./api";
import { useApi } from "./api-context";
import { createPasskey, defaultPasskeyName } from "./webauthn";

export const passkeysKey = ["passkeys"] as const;

/** Creates a passkey on this device and registers it for the signed-in person. */
export function useAddPasskey() {
  const api = useApi();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const ceremony = await call(api.client.POST("/api/v1/me/passkeys/options"));
      const credential = await createPasskey(ceremony.publicKey);
      const name = defaultPasskeyName();
      return call(
        api.client.POST("/api/v1/me/passkeys", {
          body: { ceremonyId: ceremony.ceremonyId, credential, ...(name ? { name } : {}) },
        }),
      );
    },
    onSuccess: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: passkeysKey }),
        queryClient.invalidateQueries({ queryKey: meKey, exact: true }),
      ]);
    },
  });
}
