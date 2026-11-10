// Scenarios: numbered test cases. Each names what it proves (covers) and is a short script of suites and steps.
// Order matters: scenarios share one seeded center, so ones that change the whole fleet or the password come last.
export function scenarios({ T, S }) {
  return [
    { id: 'SC-AUTH-01', area: 'auth', title: 'signing in needs the right password', covers: 'sign-in rules, hidden menu before sign-in', run: S.auth.signInRules },

    { id: 'SC-DASH-01', area: 'dashboard', title: 'the dashboard shows the seeded fleet', covers: 'fleet tiles, node cards, IP change count', run: S.dashboard.showsFleet },
    { id: 'SC-DASH-02', area: 'dashboard', title: 'the comparison chart offers every metric, table view and node toggle', covers: 'dashboard compare card', run: S.dashboard.compareConnections },
    { id: 'SC-DASH-03', area: 'dashboard', title: 'dashboard shortcuts lead to the right pages and dialogs', covers: 'navigation links, add node, run all', run: S.dashboard.shortcuts },
    { id: 'SC-DASH-04', area: 'dashboard', title: 'the time range in the top bar drives the pages', covers: 'time range', run: S.dashboard.timeRange },
    { id: 'SC-DASH-05', area: 'dashboard', title: 'the period comparison recovers after a temporary failure', covers: 'comparison error state and refresh retry', run: S.dashboard.comparisonRecoversFromFailure },
    { id: 'SC-DASH-06', area: 'dashboard', title: 'KPI values and previous-period deltas use exact values and directions', covers: 'download, upload, latency, success and IP change values; zero IP baseline', run: S.dashboard.comparesExactKpis },
    { id: 'SC-DASH-07', area: 'dashboard', title: 'Enter opens a node card and its Run button without activating both', covers: 'node card and nested Run keyboard behavior', run: S.dashboard.keyboardCardDoesNotCatchRunEnter },
    { id: 'SC-DASH-08', area: 'dashboard', title: 'delayed comparison success and failure preserve node edits and focus', covers: 'stale dashboard responses after navigation', run: S.dashboard.delayedComparisonKeepsNodeEdit },
    { id: 'SC-DASH-09', area: 'dashboard', title: 'result rows open details by row and by named keyboard action', covers: 'dashboard result table row and link actions', run: S.dashboard.dashboardResultActions },
    { id: 'SC-DASH-10', area: 'dashboard', title: 'dashboard alerts can be acknowledged in demo mode', covers: 'dashboard alert acknowledge action', run: S.dashboard.dashboardAlertCanBeAcknowledgedInDemo },

    { id: 'SC-QA-01', area: 'quality', title: 'every page draws real data without console errors', covers: 'all routes', run: S.quality.everyPageDraws },
    { id: 'SC-QA-02', area: 'quality', title: 'every page holds its layout at 390, 768 and 1440 px in both themes', covers: 'responsive layout, themes', run: S.quality.layoutHolds },
    { id: 'SC-QA-03', area: 'quality', title: 'controls have names and the keyboard reaches them on every page', covers: 'accessibility basics', run: S.quality.accessibility },
    { id: 'SC-QA-04', area: 'quality', title: 'the theme switch persists', covers: 'theme preference stored on the center', run: S.quality.themeSwitch },
    { id: 'SC-QA-05', area: 'quality', title: 'on a phone the top bar fits one row and remaining actions work in the menu', covers: '390 px top bar, active-node status, theme and sign out menu actions, download icon', run: S.quality.phoneTopBar },
    { id: 'SC-QA-06', area: 'quality', title: 'dashboard and Statistics explain sparse trends and expected lines stay visible', covers: 'zero, one, two and three measurements; engine/node filtering; hidden series; empty and populated expected-speed lines', run: S.nodes.sparseTrendsExplainThreshold },
    { id: 'SC-QA-07', area: 'quality', title: 'Statistics counts plotted buckets for sparse trends in demo and API modes', covers: 'three samples in one bucket and five samples in two buckets in both modes', run: S.nodes.sparseWarningsCountBuckets },
    { id: 'SC-QA-08', area: 'quality', title: 'shared UI helpers apply attributes and keep copy and overlay actions operable', covers: 'dataset, node tag, clipboard fallback, modal close and drawer close', run: S.quality.sharedUiActionsAreOperable },

    { id: 'SC-NODE-01', area: 'nodes', title: 'the node list opens by keyboard and Enter on Run does not open its row', covers: 'nodes list, detail, row keyboard navigation, nested Run action', run: S.nodes.listSortsAndOpens },
    { id: 'SC-NODE-09', area: 'nodes', title: 'the node list shows download and upload against the expected speed', covers: 'nodes list upload column', run: S.nodes.listShowsUpload },
    { id: 'SC-NODE-10', area: 'nodes', title: 'a sparse speed chart explains the data limit and expected speeds', covers: 'sparse chart note and expected download/upload references', run: S.nodes.sparseChartShowsReference },
    { id: 'SC-NODE-07', area: 'nodes', title: 'the node list is a list of cards on a phone', covers: 'mobile layout without sideways scrolling', run: S.nodes.cardsOnPhone },
    { id: 'SC-NODE-02', area: 'nodes', title: 'the add-node wizard refuses incomplete and duplicate input', covers: 'wizard validation', run: S.nodes.wizardValidation },
    { id: 'SC-NODE-03', area: 'nodes', title: 'node settings validate, save and refuse a duplicate name', covers: 'node edit', run: S.nodes.detailAndSettings },
    { id: 'SC-NODE-08', area: 'nodes', title: 'a published release enables the agent update action', covers: 'release manifest, owner indicator, update command', run: S.nodes.publishedUpdate },
    { id: 'SC-NODE-04', area: 'nodes', title: 'a node can be paused and resumed', covers: 'pause/resume', run: S.nodes.pauseAndResume },

    { id: 'SC-RES-01', area: 'results', title: 'results can be filtered, opened and exported', covers: 'results filter, detail, CSV export', run: S.results.filterOpenExport },
    { id: 'SC-RES-02', area: 'results', title: 'results sort, page and filter by node and speed', covers: 'results sorting, paging, chips', run: S.results.sortPageAndChips },

    { id: 'SC-STAT-01', area: 'statistics', title: 'statistics show the center figures', covers: '/api/stats summary on the page', run: S.statistics.summaryMatchesApi },
    { id: 'SC-STAT-02', area: 'statistics', title: 'every statistics control works', covers: 'metric tabs, chips, combined total, table, heat map, range', run: S.statistics.everyControl },
    { id: 'SC-STAT-03', area: 'statistics', title: 'statistics explains an API failure and recovers after retry', covers: 'remote statistics failure callback and reload recovery', run: S.statistics.unavailableTrendExplainsRetry },

    { id: 'SC-IP-01', area: 'ip', title: 'the map, lookup and address details work', covers: 'map, lookup, details', run: S.network.mapLookupAndDetails },
    { id: 'SC-IP-02', area: 'ip', title: 'the IP page filters by node and lists address changes', covers: 'node filter, change timeline', run: S.network.filterAndHistory },

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
    { id: 'SC-SET-07', area: 'settings', title: 'notification quiet hours are saved as a complete window', covers: 'quiet hours for messages, critical exemption', run: S.settings.notificationQuietHours },
    { id: 'SC-SET-05', area: 'settings', title: 'retention and the full exports work', covers: 'retention, CSV and JSON export', run: S.settings.dataRetentionAndExport },
    { id: 'SC-SET-06', area: 'settings', title: 'the enrolment key is shown once', covers: 'enrolment key rotation', run: S.settings.enrolmentKeyShownOnce },

    // Adds a real agent and a node; the removal scenario below deletes it again.
    { id: 'SC-NODE-05', area: 'nodes', title: 'a node is added with the install command and reports', covers: 'wizard, real agent enrolment, first result, run test', run: S.nodes.addWithCommandAndRunTest },
    { id: 'SC-NODE-06', area: 'nodes', title: 'a node can be removed from the dashboard', covers: 'node removal', run: () => S.nodes.removeFromDashboard('E2E-1') },

    { id: 'SC-SET-08', area: 'settings', title: 'the activity log lists owner changes without secrets', covers: 'audit log, filter, no secrets', run: S.settings.activityLog },

    { id: 'SC-AUTH-02', area: 'auth', title: 'the owner can change the password', covers: 'change password, old password refused, new one works', run: () => S.auth.changePassword('a brand new passphrase 2026') },
    { id: 'SC-AUTH-04', area: 'auth', title: 'two-factor sign-in can be turned on, used and turned off', covers: 'authenticator setup, code at sign-in, recovery code once, turn off', run: () => S.auth.twoFactor(process.env.E2E_ONLY ? undefined : 'a brand new passphrase 2026') },
    { id: 'SC-AUTH-03', area: 'auth', title: 'signing out ends the session', covers: 'sign out, API refuses visitor', run: S.auth.signOutEndsSession },
  ];
}
