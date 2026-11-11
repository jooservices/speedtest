// Scenarios: numbered test cases. Each names what it proves (covers) and is a short script of suites and steps.
// Order matters: scenarios share one seeded center, so ones that change the whole fleet or the password come last.
export function scenarios({ T, S }) {
  return [
    { id: 'SC-AUTH-01', area: 'auth', title: 'signing in needs the right password', covers: 'sign-in rules, hidden menu before sign-in', run: S.auth.signInRules },

    { id: 'SC-DASH-01', area: 'dashboard', title: 'the dashboard shows the seeded fleet', covers: 'fleet tiles, node cards, IP change count', run: S.dashboard.showsFleet },
    { id: 'SC-DASH-02', area: 'dashboard', title: 'the comparison chart offers every metric, table view and node toggle', covers: 'dashboard compare card', run: S.dashboard.compareConnections },
    { id: 'SC-DASH-03', area: 'dashboard', title: 'dashboard shortcuts lead to the right pages and dialogs', covers: 'navigation links, add node, run all', run: S.dashboard.shortcuts },
    { id: 'SC-DASH-04', area: 'dashboard', title: 'the time range in the top bar drives the pages', covers: 'time range', run: S.dashboard.timeRange },
    { id: 'SC-DASH-05', area: 'dashboard', title: 'rate-limited tests stay visible and do not distort success rates', covers: 'dashboard KPI, Statistics table and node detail for mixed and all-rate-limited API data; keyboard/touch navigation and responsive dashboard count', run: S.dashboard.rateLimitedCount },

    { id: 'SC-QA-01', area: 'quality', title: 'every page draws real data without console errors', covers: 'all routes', run: S.quality.everyPageDraws },
    { id: 'SC-QA-02', area: 'quality', title: 'every page holds its layout at 390, 768 and 1440 px in both themes', covers: 'responsive layout, themes', run: S.quality.layoutHolds },
    { id: 'SC-QA-03', area: 'quality', title: 'controls have names and the keyboard reaches them on every page', covers: 'accessibility basics', run: S.quality.accessibility },
    { id: 'SC-QA-04', area: 'quality', title: 'the theme switch persists', covers: 'theme preference stored on the center', run: S.quality.themeSwitch },
    { id: 'SC-QA-05', area: 'quality', title: 'on a phone the top bar fits one row and download buttons show a download icon', covers: 'top bar layout at 390 px, download icon', run: S.quality.phoneTopBar },

    { id: 'SC-NODE-01', area: 'nodes', title: 'the node list sorts and opens a node', covers: 'nodes list, detail', run: S.nodes.listSortsAndOpens },
    { id: 'SC-NODE-07', area: 'nodes', title: 'the node list is a list of cards on a phone', covers: 'mobile layout without sideways scrolling', run: S.nodes.cardsOnPhone },
    { id: 'SC-NODE-02', area: 'nodes', title: 'the add-node wizard refuses incomplete and duplicate input', covers: 'wizard validation', run: S.nodes.wizardValidation },
    { id: 'SC-NODE-03', area: 'nodes', title: 'node settings validate, save and refuse a duplicate name', covers: 'node edit', run: S.nodes.detailAndSettings },
    { id: 'SC-NODE-08', area: 'nodes', title: 'a published release enables the agent update action', covers: 'release manifest, owner indicator, update command', run: S.nodes.publishedUpdate },
    { id: 'SC-NODE-04', area: 'nodes', title: 'a node can be paused and resumed', covers: 'pause/resume', run: S.nodes.pauseAndResume },

    { id: 'SC-RES-01', area: 'results', title: 'results can be filtered, opened and exported', covers: 'results filter, detail, CSV export', run: S.results.filterOpenExport },
    { id: 'SC-RES-02', area: 'results', title: 'results sort, page and filter by node and speed', covers: 'results sorting, paging, chips', run: S.results.sortPageAndChips },
    { id: 'SC-RES-03', area: 'results', title: 'failed results show a short failure-class chip', covers: 'JOOST-73 failureClass reason chip on Results', run: S.results.failureClassChip },

    { id: 'SC-STAT-01', area: 'statistics', title: 'statistics show the center figures', covers: '/api/stats summary on the page', run: S.statistics.summaryMatchesApi },
    { id: 'SC-STAT-02', area: 'statistics', title: 'every statistics control works', covers: 'metric tabs, chips, combined total, table, heat map, range', run: S.statistics.everyControl },
    { id: 'SC-STAT-03', area: 'statistics', title: 'demo statistics explain excluded rate-limited tests', covers: 'demo Statistics table and node detail show mixed and all-rate-limited counts', run: S.statistics.demoRateLimitExclusions },

    { id: 'SC-IP-01', area: 'ip', title: 'the map, lookup and address details work', covers: 'map, lookup, details', run: S.network.mapLookupAndDetails },
    { id: 'SC-IP-02', area: 'ip', title: 'the IP page filters by node and lists address changes', covers: 'node filter, change timeline', run: S.network.filterAndHistory },

    { id: 'SC-ISP-01', area: 'isps', title: 'the ISP page lists providers and supports compare and filters', covers: 'ISP list, trend, compare, results isps filter', run: S.isps.listAndCompare },
    { id: 'SC-ISP-02', area: 'isps', title: 'ISP filters stay local to Results, Statistics and Alerts', covers: 'multi-ISP predicates, clear filters, dashboard snapshot, map and full exports', run: S.isps.filtersStayPageScopedAndClear },
    { id: 'SC-ISP-03', area: 'isps', title: 'ISP plan defaults update while explicit speeds stay fixed', covers: 'plan assignment, equal and different speed overrides, later plan edits', run: S.isps.planDefaultsPreserveOverrides },
    { id: 'SC-ISP-04', area: 'isps', title: 'the monthly ISP report previews known metrics and downloads Unicode CSV and PDF', covers: 'JOOST-79 known monthly data, preview metrics, validation, month picker, out-of-order month responses, CSV and PDF downloads', run: S.isps.monthlyReport },

    { id: 'SC-ENG-01', area: 'engines', title: 'a second installed engine has an independent saved schedule', covers: 'JOOST-71 capabilities, enable/disable, 60-minute default and persistence', run: S.engines.enableSecondEngine },
    { id: 'SC-ENG-02', area: 'engines', title: 'the engine selector filters charts, results and statistics', covers: 'JOOST-71 engine-isolated views and empty states', run: S.engines.selectEngine },

    { id: 'SC-SCH-01', area: 'schedules', title: 'schedules are saved on the center', covers: 'interval, stagger, quiet hours', run: S.schedules.intervalStaggerQuietHours },
    { id: 'SC-SCH-02', area: 'schedules', title: 'per-node schedule controls and global rules work', covers: 'offset, active switch, run now, overlap rule, concurrency', run: S.schedules.perNodeControls },

    { id: 'SC-ALR-01', area: 'alerts', title: 'alert rules can be created, edited, switched and deleted', covers: 'alert rule lifecycle', run: S.alerts.ruleLifecycle },
    { id: 'SC-ALR-02', area: 'alerts', title: 'alert rules are validated and every rule type is offered', covers: 'rule validation, rule types', run: S.alerts.ruleValidationAndTypes },
    { id: 'SC-ALR-03', area: 'alerts', title: 'alerts can be acknowledged, reopened and filtered', covers: 'ack, reopen, ack all, filters', run: S.alerts.activityAcknowledge },

    { id: 'SC-SET-01', area: 'settings', title: 'preferences are saved and survive a reload', covers: 'decimals, units, time zone, 12-hour, range, theme', run: S.settings.preferences },
    { id: 'SC-SET-02', area: 'settings', title: 'speed test and location settings are kept; the licence key is write-only', covers: 'engine, location', run: S.settings.engineAndLocation },
    { id: 'SC-SET-03', area: 'settings', title: 'notification secrets are write-only and encrypted', covers: 'secrets', run: S.settings.notificationSecrets },
    { id: 'SC-SET-04', area: 'settings', title: 'a test notification reaches the configured channel', covers: 'webhook test, unset channel refused', run: S.settings.notificationTest },
    { id: 'SC-SET-09', area: 'settings', title: 'notification events and templates can be configured globally and per node', covers: 'event catalog toggles, quality consecutive-test threshold, per-node override, template editor, live preview and reset', run: S.settings.notificationEventCatalog },
    { id: 'SC-SET-07', area: 'settings', title: 'notification quiet hours are saved as a complete window', covers: 'quiet hours for messages, critical exemption', run: S.settings.notificationQuietHours },
    { id: 'SC-SET-05', area: 'settings', title: 'retention and the full exports work', covers: 'retention, CSV and JSON export', run: S.settings.dataRetentionAndExport },
    { id: 'SC-SET-06', area: 'settings', title: 'the enrolment key is shown once', covers: 'enrolment key rotation', run: S.settings.enrolmentKeyShownOnce },
    { id: 'SC-SET-10', area: 'settings', title: 'API access tokens are issued once, authorize scoped calls and can be revoked', covers: 'token creation, digest storage, bearer auth without cookies or CSRF, scope enforcement, installation event stream revalidation, revocation', run: S.settings.apiTokenCanBeCreatedUsedAndRevoked },

    // Adds a real agent and a node; the removal scenario below deletes it again.
    { id: 'SC-NODE-05', area: 'nodes', title: 'a node is added with the install command, reports and recovers from provider limits', covers: 'wizard, 60-minute default, real agent enrolment/sync, test-success and manual notifications, rule failure/recovery delivery, node offline/online recovery with downtime, rate-limit classification and restart backoff', run: S.nodes.addWithCommandAndRunTest },
    { id: 'SC-NODE-06', area: 'nodes', title: 'a node can be removed from the dashboard', covers: 'node removal', run: () => S.nodes.removeFromDashboard('E2E-1') },

    { id: 'SC-SET-08', area: 'settings', title: 'the activity log lists owner changes without secrets', covers: 'audit log, filter, no secrets', run: S.settings.activityLog },

    { id: 'SC-AUTH-02', area: 'auth', title: 'the owner can change the password', covers: 'change password, old password refused, new one works', run: () => S.auth.changePassword('a brand new passphrase 2026') },
    { id: 'SC-AUTH-04', area: 'auth', title: 'two-factor sign-in can be turned on, used and turned off', covers: 'authenticator setup, code at sign-in, recovery code once, turn off', run: () => S.auth.twoFactor(process.env.E2E_ONLY ? undefined : 'a brand new passphrase 2026') },
    { id: 'SC-AUTH-03', area: 'auth', title: 'signing out ends the session', covers: 'sign out, API refuses visitor', run: S.auth.signOutEndsSession },
    { id: 'SC-NODE-09', area: 'nodes', title: 'demo-mode nodes start with a 60-minute schedule and zero offset', covers: 'demo wizard, new-node default, schedules display', run: S.nodes.demoNodeDefault },
  ];
}
