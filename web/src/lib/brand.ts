/** The product name. Brand names are not translated. */
export const productName = "Orglane";

/** The workspace's host as people know it, e.g. "work.acme.com". */
export function workspaceHost(): string {
  return typeof window === "undefined" ? "" : window.location.host;
}
