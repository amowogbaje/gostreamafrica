// Starts Swagger UI against this server's own spec. Kept in a separate file (not inline) so the
// page can run under a strict Content-Security-Policy.
(function () {
  // The page lives at <prefix>/docs/ : the prefix is "" when the API is opened directly and "/api"
  // behind nginx. "Try it out" must call the SAME origin and prefix so it needs no CORS and the
  // refresh cookie is sent.
  var prefix = location.pathname.replace(/\/docs\/?$/, '');
  var target = document.getElementById('swagger-ui');

  function fail(message) {
    var p = document.createElement('p');
    p.style.cssText = 'font:16px system-ui,sans-serif;padding:1rem';
    p.textContent = message;
    target.appendChild(p);
  }

  fetch('openapi.json', { credentials: 'same-origin' })
    .then(function (res) {
      if (!res.ok) { throw new Error('HTTP ' + res.status); }
      return res.json();
    })
    .then(function (spec) {
      spec.servers = [{ url: location.origin + prefix, description: 'this server' }];
      window.ui = SwaggerUIBundle({
        spec: spec,
        dom_id: '#swagger-ui',
        deepLinking: true,
        persistAuthorization: true,
        displayRequestDuration: true,
        docExpansion: 'list',
        defaultModelsExpandDepth: 0,
        presets: [SwaggerUIBundle.presets.apis]
      });
    })
    .catch(function (err) { fail('Could not load the API spec: ' + err.message); });
})();
