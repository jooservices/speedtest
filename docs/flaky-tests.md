# Flaky tests

| Test | Symptom | Cause | Mitigation | Date |
| --- | --- | --- | --- | --- |
| `TestAgentIngestionLoad1000PerMinuteIntegration` | Measured center CPU varies around its 5% limit (about 4.7–5.05%). | CPU noise on a busy shared machine can move the measurement across the narrow limit. | Run it alone when machine load is below 6 and retry once. Treat a quiet measurement above 5.5% as a regression; do not raise the test limit. | 2026-11-12 |
| E2E scenario `SC-NODE-05` | Back-off timing can make the scenario outcome timing-sensitive. | The scenario depends on the back-off window elapsing before its assertion. | Use the fixed 90-second back-off window for this scenario. | 2026-11-12 |
