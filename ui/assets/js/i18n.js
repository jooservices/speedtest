/* Shared translation lookup and locale-aware formatting for the browser UI. */
(function () {
  'use strict';
  var ST = (/** @type {any} */ (window)).ST = (/** @type {any} */ (window)).ST || {};
  var language = 'en';
  var readSeq = 0;
  var appliedReadSeq = 0;
  var saveSeq = 0;
  var formatters = new Map();
  var dictionaries = {
    en: {},
    vi: { 'Could not save language': 'Không thể lưu ngôn ngữ' }
  };
  var dateFormats = {
    day: { day: '2-digit', month: 'short' },
    time: { hour: '2-digit', minute: '2-digit' },
    full: { day: '2-digit', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' },
    dayfull: { weekday: 'short', day: '2-digit', month: 'short' },
    calendarDate: { year: 'numeric', month: '2-digit', day: '2-digit' },
    yearMonth: { year: 'numeric', month: '2-digit' },
    default: { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit' }
  };

  function locale(type) {
    if (language === 'vi') return 'vi-VN';
    return type === 'date' ? 'en-GB' : 'en-US';
  }
  function formatter(type, options) {
    var localeName = locale(type);
    var key = localeName + ':' + type + ':' + JSON.stringify(options);
    if (!formatters.has(key)) {
      var instance = type === 'date'
        ? new Intl.DateTimeFormat(localeName, options)
        : new Intl.NumberFormat(localeName, options);
      formatters.set(key, instance);
    }
    return formatters.get(key);
  }
  function dateOptions(options) {
    var result = Object.assign({}, dateFormats[options.kind] || dateFormats.default);
    if (options.kind === 'yearMonth' && language === 'vi') result.month = 'long';
    if (options.timeZone) result.timeZone = options.timeZone;
    if (options.hour12 != null) result.hour12 = options.hour12;
    return result;
  }

  var i18n = /** @type {any} */ (ST.i18n = {});
  i18n.lang = function () { return language; };
  i18n.setLanguage = function (value) {
    if (value !== 'en' && value !== 'vi') throw new RangeError('Unsupported interface language.');
    language = value;
    if (typeof document !== 'undefined' && document.documentElement) document.documentElement.lang = language;
    return language;
  };
  i18n.beginLanguageRead = function () { return ++readSeq; };
  i18n.applyLanguageRead = function (value, sequence) {
    if (sequence <= appliedReadSeq) return false;
    if (value !== 'en' && value !== 'vi') throw new RangeError('Unsupported interface language.');
    appliedReadSeq = sequence;
    i18n.setLanguage(value);
    return true;
  };
  i18n.saveLanguage = function (value, persist, readLanguage, onReadFailure) {
    if (value !== 'en' && value !== 'vi') return Promise.reject(new RangeError('Unsupported interface language.'));
    if (typeof persist !== 'function') return Promise.reject(new TypeError('A language persistence function is required.'));
    if (typeof readLanguage !== 'function') return Promise.reject(new TypeError('A language read function is required.'));
    var request = ++saveSeq;
    return Promise.resolve().then(function () { return persist(value); }).then(function (result) {
      return Promise.resolve().then(readLanguage).then(function () {
        return { stale: false, data: result };
      }, function () {
        if (request === saveSeq) {
          var savedLanguage = result && (result.language === 'en' || result.language === 'vi') ? result.language : value;
          i18n.setLanguage(savedLanguage);
          if (onReadFailure) onReadFailure(savedLanguage);
        }
        return { stale: false, data: result };
      });
    }, function (error) {
      return Promise.resolve().then(readLanguage).catch(function () {}).then(function () {
        if (request !== saveSeq) return { stale: true };
        throw error;
      });
    });
  };
  i18n.t = function (key, values) {
    var dictionary = dictionaries[language];
    var translated = Object.prototype.hasOwnProperty.call(dictionary, key) ? dictionary[key] : key;
    return translated.replace(/\{([A-Za-z][A-Za-z0-9_]*)\}/g, function (placeholder, name) {
      return values && Object.prototype.hasOwnProperty.call(values, name) ? String(values[name]) : placeholder;
    });
  };
  i18n.localize = function (value, options) {
    options = options || {};
    if (value == null) return '—';
    if (options.type === 'identifier') return String(value);
    if (options.type === 'date') return formatter('date', dateOptions(options)).format(new Date(value));
    if (options.type === 'number') {
      var numberOptions = Object.assign({}, options);
      delete numberOptions.type;
      return formatter('number', numberOptions).format(Number(value));
    }
    throw new TypeError('Unsupported localization type.');
  };
})();
