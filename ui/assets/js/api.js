/* Same-origin JSON transport. Session secrets and cached responses stay in memory. */
(function () {
  'use strict';
  var ST = window.ST = window.ST || {};
  var cache = new Map(), csrf = null, generation = 0;
  var API = ST.api = {};

  function problem(status, detail, fields) {
    var error = new Error(detail);
    error.status = status;
    return Object.assign(error, fields || {});
  }
  API.signIn = function () {
    csrf = null;
    API.clear();
    if (location.hash !== '#/signin') location.hash = '#/signin';
  };
  API.clear = function () { generation++; cache.clear(); };
  API.key = function () { return crypto.randomUUID(); };

  API.request = async function (path, options) {
    options = options || {};
    // Accept only API paths on this origin, including when callers supply user input.
    if (!/^\/api\//.test(path) || /[\\\r\n#]/.test(path) || new URL(path, location.href).origin !== location.origin) {
      throw problem(0, 'Invalid API path.');
    }
    var method = (options.method || 'GET').toUpperCase();
    var read = method === 'GET', auth = path.indexOf('/api/auth/') === 0;
    var ttl = read && !auth ? Math.min(Math.max(options.cacheMs || 0, 0), 5000) : 0;
    var cached = cache.get(path), version = generation;
    if (options.signal) options.signal.throwIfAborted();
    if (ttl && cached && cached.until > Date.now()) return structuredClone(cached.value);
    var headers = new Headers(options.headers);
    headers.set('Accept', 'application/json');
    if (!read && method !== 'HEAD') {
      if (!csrf) await API.session({ signal: options.signal });
      headers.set('X-CSRF-Token', csrf);
    }
    if (options.idempotencyKey) headers.set('Idempotency-Key', options.idempotencyKey);
    if (options.ifMatch) headers.set('If-Match', options.ifMatch);
    var body;
    if (options.body !== undefined) {
      headers.set('Content-Type', options.contentType || 'application/json');
      body = JSON.stringify(options.body);
    }
    var response;
    try {
      response = await fetch(path, { method: method, headers: headers, body: body,
        credentials: 'same-origin', cache: 'no-store', signal: options.signal });
    } catch (error) {
      if (error.name === 'AbortError' || (options.signal && options.signal.aborted)) throw error;
      throw problem(0, 'Cannot reach the command center. Check your connection and retry.');
    }
    if (response.status === 401) API.signIn();
    if (response.status === 204 || response.status === 304) {
      if (!read) { csrf = null; API.clear(); }
      return { data: null, meta: {}, etag: response.headers.get('ETag'), status: response.status };
    }
    var json;
    try { json = await response.json(); }
    catch (error) {
      throw problem(response.status, 'The command center returned invalid JSON. Retry or contact the operator.');
    }
    if (!response.ok) {
      if (response.status === 403) csrf = null;
      json = json || {};
      throw problem(response.status, json.detail || json.title || 'Request failed. Please retry.', {
        type: json.type, errors: json.errors, traceId: json.trace_id,
        retryAfter: response.headers.get('Retry-After')
      });
    }
    if (!json || json.success !== true || json.code !== response.status || !('data' in json) || !json.meta || json.errors !== null) {
      throw problem(response.status, 'The command center returned an invalid response. Please retry.');
    }
    var result = { data: json.data, meta: json.meta, etag: response.headers.get('ETag'), status: response.status };
    if (auth && result.data && result.data.csrfToken) csrf = result.data.csrfToken;
    if (!read) API.clear();
    if (ttl && version === generation) {
      if (cache.size >= 100) cache.delete(cache.keys().next().value);
      cache.set(path, { until: Date.now() + ttl, value: structuredClone(result) });
    }
    return result;
  };
  API.session = function (options) { return API.request('/api/auth/me', options); };
  API.list = async function (path, options) {
    var rows = [], page = 1, last = 1;
    do {
      var url = new URL(path, location.href);
      url.searchParams.set('page', page);
      url.searchParams.set('size', 100);
      var result = await API.request(url.pathname + url.search, options);
      var pagination = result.meta.pagination;
      if (!Array.isArray(result.data) || !pagination || pagination.current_page !== page ||
          !Number.isInteger(pagination.last_page) || pagination.last_page < 1) {
        throw problem(0, 'Invalid pagination from the command center. Please retry.');
      }
      last = pagination.last_page;
      // Never silently display partial data or allow a malformed server to exhaust the browser.
      if (last > 1000) throw problem(0, 'Too much data for this view. Choose a shorter time range and retry.');
      rows = rows.concat(result.data);
      page++;
    } while (page <= last);
    return rows;
  };
})();
