/* Sign-in page and sign-out. The session lives in an HttpOnly cookie; nothing secret is kept in the page. */
(function () {
  'use strict';
  var ST = window.ST, U = ST.ui, h = U.h, API = ST.api;

  // Plain-language text for what the center can answer, never the raw server detail.
  function explain(error) {
    if (error.status === 401 || error.status === 422) return 'The email or password is not right. Check them and try again.';
    if (error.status === 429) return 'Too many attempts. Wait a minute and try again.';
    if (error.status === 403) return 'The session expired while you were typing. Reload the page and try again.';
    if (error.status === 0) return error.message;
    return 'Sign-in is not available right now. Try again in a moment.';
  }

  ST.signOut = function () {
    return API.request('/api/auth/logout', { method: 'POST' }).catch(function () { /* the cookie is cleared by the server either way */ })
      .then(function () { API.signIn(); ST.cancelLoad && ST.cancelLoad(); ST.emit('remote'); });
  };

  ST.pages.signin = function (root) {
    var state = { busy: false };
    var email = U.input('', null, { type: 'email', autocomplete: 'username', required: true, id: 'signin-email', name: 'email' });
    var password = U.input('', null, { type: 'password', autocomplete: 'current-password', required: true, id: 'signin-password', name: 'password' });
    var code = U.input('', null, { type: 'text', inputmode: 'text', autocomplete: 'one-time-code', id: 'signin-code', name: 'code', maxlength: 32 });
    var codeField = U.field('Authentication code', code, 'The 6-digit code from your authenticator app, or one of your recovery codes.');
    codeField.style.display = 'none';
    var message = h('div', { class: 'form-error', role: 'alert', 'aria-live': 'polite' });
    var submit = h('button', { type: 'submit', class: 'btn primary' }, 'Sign in');
    function show(text) { message.textContent = text || ''; message.style.display = text ? '' : 'none'; }
    show('');
    var form = h('form', { class: 'stack', novalidate: true, 'aria-label': 'Sign in', onSubmit: function (event) {
      event.preventDefault();
      if (state.busy) return;
      if (!email.value.trim() || !password.value) { show('Enter your email and password.'); (email.value.trim() ? password : email).focus(); return; }
      var asking = codeField.style.display !== 'none';
      if (asking && !code.value.trim()) { show('Enter the code from your authenticator app.'); code.focus(); return; }
      state.busy = true; submit.disabled = true; submit.textContent = 'Signing in…'; show('');
      var body = { email: email.value.trim(), password: password.value };
      if (asking) body.code = code.value.trim();
      API.request('/api/auth/login', { method: 'POST', body: body }).then(function () {
        password.value = ''; code.value = '';
        location.hash = '#/';
        return ST.load();
      }).catch(function (error) {
        if (error.type === '/problems/two-factor-required') {
          // The password was right; the second step asks for the code and keeps the password in the form.
          codeField.style.display = ''; show('Enter the code from your authenticator app to finish signing in.'); code.focus();
          return;
        }
        if (asking && error.status === 401) { show('That code is not right, or it was already used. Try the next code.'); code.value = ''; code.focus(); return; }
        show(explain(error)); password.value = ''; code.value = ''; password.focus();
      }).then(function () { state.busy = false; submit.disabled = false; submit.textContent = 'Sign in'; });
    } },
      U.field('Email', email, null), U.field('Password', password, null), codeField, message, submit);
    root.appendChild(h('div', { class: 'signin' },
      h('div', { class: 'card card-pad signin-card' },
        h('div', { class: 'brand', style: 'margin-bottom:12px' }, h('div', { class: 'brand-mark' }, U.icon('gauge', 18)),
          h('div', null, h('div', { class: 'brand-name' }, 'Speedtest'), h('div', { class: 'brand-sub' }, 'Network monitor'))),
        h('h1', { class: 'page-title', style: 'font-size:20px;margin:0 0 4px' }, 'Sign in'),
        h('p', { class: 'muted', style: 'margin:0 0 14px' }, 'Sign in to see your connections.'),
        form)));
    setTimeout(function () { email.focus(); }, 0);
  };
})();
