// Swagger UI bootstrap. Kept in a separate file because the CSP forbids inline scripts.
"use strict";
window.addEventListener("DOMContentLoaded", function () {
  // Credentials typed into "Authorize" (session tokens, personal access tokens) live only in
  // memory for this page. Persisting them would put them in localStorage on the web UI's own
  // origin, where they outlive sign-out (shared computers) and any same-origin script can read
  // them. Earlier versions did persist them: remove what they left behind.
  try {
    window.localStorage.removeItem("authorized");
  } catch (e) {
    // Storage unavailable (private mode, blocked site data): nothing was persisted either.
  }
  window.ui = window.SwaggerUIBundle({
    url: "/api/openapi.json",
    dom_id: "#swagger-ui",
    deepLinking: true,
    persistAuthorization: false,
    displayRequestDuration: true,
    filter: true,
    docExpansion: "none",
    // Never contact the public validator service (privacy; also blocked by CSP).
    validatorUrl: null,
    presets: [window.SwaggerUIBundle.presets.apis],
    layout: "BaseLayout"
  });
});
