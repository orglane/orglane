// Runs only in browsers that cannot run the web app (no JavaScript modules:
// <script nomodule>), such as the HtmlUnit browser of the OpenID Conformance
// Suite. On the sign-in screen of an application's sign-in (OIDC), it shows
// the basic password form that index.html carries (hidden), which posts to
// /oidc/login. Uses only syntax such browsers run (as theme-init.js does).
(() => {
  const form = document.getElementById("basic-sign-in");
  const params = new URLSearchParams(window.location.search);
  const authRequest = params.get("authRequest");
  if (!form || window.location.pathname !== "/sign-in" || !authRequest) {
    return;
  }
  form.elements.authRequest.value = authRequest;
  form.elements.email.value = params.get("email") || "";
  const error = params.get("error");
  const message = error
    ? form.querySelector(`[data-error="${error}"]`) || form.querySelector('[data-error="failed"]')
    : null;
  if (message) {
    message.hidden = false;
  }
  form.hidden = false;
})();
