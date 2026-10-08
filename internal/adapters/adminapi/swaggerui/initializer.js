// Starts Swagger UI on the contract this node serves. Nothing is loaded from
// anywhere else: no validator, no fonts, no remote specification.
window.addEventListener('load', function () {
  window.ui = SwaggerUIBundle({
    url: '/api/v1/openapi.json',
    dom_id: '#swagger-ui',
    validatorUrl: null,
    deepLinking: true,
    presets: [SwaggerUIBundle.presets.apis],
    layout: 'BaseLayout',
    // An API key typed into "Authorize" lives in this page only.
    persistAuthorization: false,
    tryItOutEnabled: true
  });
});
