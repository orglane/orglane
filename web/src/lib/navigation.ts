/** Full-page navigation (leaving the app), replaceable in tests. */
export const browserNavigation = {
  assign(url: string): void {
    window.location.assign(url);
  },
};
