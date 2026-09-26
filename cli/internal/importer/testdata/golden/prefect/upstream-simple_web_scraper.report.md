# nexusctl import prefect report

- Input: `testdata/upstream/prefect/simple_web_scraper.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 2 approximated, 2 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## scrape -> `scrape`

Source `testdata/upstream/prefect/simple_web_scraper.py:93`. 2 task(s), 1 edge(s). 1 translated, 2 approximated, 2 unsupported. Content address `b19c7ecd9d32`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `simple_web_scraper.py:93` | @flow | fail_fast | a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish) |
| translated | `simple_web_scraper.py:94` | param | params.urls (json) | param urls |
| unsupported | `simple_web_scraper.py:100` | if | placeholders: fetch_html, parse_article | a condition creates or wires tasks (fetch_html, parse_article); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| unsupported | `simple_web_scraper.py:101` | for | placeholders: fetch_html, parse_article | a for loop creates or wires tasks (fetch_html, parse_article); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| approximated | `simple_web_scraper.py:103` | print |  | print() in the flow body does not run in Nexus: there is no flow-body process |

### After import

```sh
nexusctl workflow submit scrape.yaml --dry-run --namespace team-a
```
