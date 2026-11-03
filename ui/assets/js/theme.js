// Apply the saved theme before first paint to avoid a flash.
(function () {
  try {
    var s = {};
    if (new URLSearchParams(location.search).get('demo') === '1') {
      s = JSON.parse(localStorage.getItem('st.demo.v1') || '{}').settings || {};
    }
    var t = s.theme === 'light' ? 'light' : 'dark'; // dark is the default
    document.documentElement.setAttribute('data-theme', t);
  } catch (e) {}
})();
