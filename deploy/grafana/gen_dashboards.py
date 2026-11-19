#!/usr/bin/env python3
"""Generates the Grafana dashboard JSON in deploy/grafana/dashboards/.
Edit here, run `python3 deploy/grafana/gen_dashboards.py`, commit the output."""
import json, os

OUT = os.path.join(os.path.dirname(__file__), "dashboards")
TEMPO = {"type": "tempo", "uid": "tempo"}
PROM = {"type": "prometheus", "uid": "prometheus"}
INF = {"type": "yesoreyeram-infinity-datasource", "uid": "infinity"}

_id = 0
def pid():
    global _id; _id += 1; return _id

def grid(x, y, w, h): return {"x": x, "y": y, "w": w, "h": h}

def text_var(name, label, default):
    return {"name": name, "label": label, "type": "textbox", "query": default,
            "current": {"text": default, "value": default}}

def const_var(name, value):
    return {"name": name, "type": "constant", "hide": 2, "query": value,
            "current": {"text": value, "value": value}}

def custom_var(name, label, options, include_all=True):
    return {"name": name, "label": label, "type": "custom", "query": ",".join(options),
            "includeAll": include_all, "allValue": ".*", "multi": False,
            "current": {"text": "All", "value": "$__all"} if include_all else {"text": options[0], "value": options[0]}}

def traceql_table(title, q, pos, desc=""):
    return {"id": pid(), "type": "table", "title": title, "description": desc, "gridPos": pos,
            "datasource": TEMPO,
            "targets": [{"refId": "A", "datasource": TEMPO, "queryType": "traceql", "query": q,
                         "limit": 200, "spss": 20, "tableType": "spans"}],
            "options": {"showHeader": True, "cellHeight": "sm"},
            "fieldConfig": {"defaults": {}, "overrides": []}}

def infinity_table(title, url, pos, desc=""):
    return {"id": pid(), "type": "table", "title": title, "description": desc, "gridPos": pos,
            "datasource": INF,
            "targets": [{"refId": "A", "datasource": INF, "type": "json", "source": "url", "format": "table",
                         "parser": "backend", "url": url, "url_options": {"method": "GET"},
                         "root_selector": "", "columns": [
                             {"selector": "repo", "text": "Repo", "type": "string"},
                             {"selector": "workflow", "text": "Workflow", "type": "string"},
                             {"selector": "release_line", "text": "Line", "type": "string"},
                             {"selector": "version", "text": "Version", "type": "string"},
                             {"selector": "change_type", "text": "Type", "type": "string"},
                             {"selector": "status", "text": "Status", "type": "string"},
                             {"selector": "conclusion", "text": "Conclusion", "type": "string"},
                             {"selector": "duration_seconds", "text": "Duration", "type": "number"},
                             {"selector": "jobs_completed", "text": "Jobs done", "type": "number"},
                             {"selector": "jobs_total", "text": "Jobs seen", "type": "number"},
                             {"selector": "jobs_failed", "text": "Jobs failed", "type": "number"},
                             {"selector": "running_jobs", "text": "Running now", "type": "string"},
                             {"selector": "head_sha", "text": "SHA", "type": "string"},
                             {"selector": "started_at", "text": "Started", "type": "timestamp"},
                             {"selector": "url", "text": "URL", "type": "string"},
                         ]}],
            "options": {"showHeader": True, "cellHeight": "sm"},
            "fieldConfig": {"defaults": {}, "overrides": [
                {"matcher": {"id": "byName", "options": "Duration"},
                 "properties": [{"id": "unit", "value": "s"}]},
                {"matcher": {"id": "byName", "options": "URL"},
                 "properties": [{"id": "links", "value": [{"title": "Open in GitHub", "url": "${__value.raw}", "targetBlank": True}]}]},
                {"matcher": {"id": "byName", "options": "Conclusion"},
                 "properties": [{"id": "custom.cellOptions", "value": {"type": "color-background"}},
                                {"id": "mappings", "value": [{"type": "value", "options": {
                                    "success": {"color": "green"}, "failure": {"color": "red"},
                                    "cancelled": {"color": "orange"}, "timed_out": {"color": "red"}}}]}]},
            ]}}

def ts(title, exprs, pos, unit="short", desc="", legend=True):
    return {"id": pid(), "type": "timeseries", "title": title, "description": desc, "gridPos": pos,
            "datasource": PROM,
            "targets": [{"refId": chr(65 + i), "datasource": PROM, "expr": e, "legendFormat": l}
                        for i, (e, l) in enumerate(exprs)],
            "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
            "options": {"legend": {"showLegend": legend, "displayMode": "table", "placement": "right"},
                        "tooltip": {"mode": "multi"}}}

def stat(title, expr, pos, unit="short", thresholds=None):
    return {"id": pid(), "type": "stat", "title": title, "gridPos": pos, "datasource": PROM,
            "targets": [{"refId": "A", "datasource": PROM, "expr": expr, "instant": True}],
            "fieldConfig": {"defaults": {"unit": unit, "thresholds": thresholds or {
                "mode": "absolute", "steps": [{"color": "green", "value": None}]}}, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "colorMode": "background"}}

def bargauge(title, expr, pos, unit="s"):
    return {"id": pid(), "type": "bargauge", "title": title, "gridPos": pos, "datasource": PROM,
            "targets": [{"refId": "A", "datasource": PROM, "expr": expr, "instant": True, "legendFormat": "{{repo}} · {{job}}"}],
            "fieldConfig": {"defaults": {"unit": unit}, "overrides": []},
            "options": {"orientation": "horizontal", "displayMode": "gradient", "reduceOptions": {"calcs": ["lastNotNull"]}}}

def row(title, y): return {"id": pid(), "type": "row", "title": title, "gridPos": grid(0, y, 24, 1), "collapsed": False, "panels": []}

def dash(uid, title, panels, variables, time_from, tags, links=None, refresh=""):
    return {"uid": uid, "title": title, "tags": tags, "timezone": "browser", "schemaVersion": 39,
            "editable": True, "refresh": refresh,
            "time": {"from": time_from, "to": "now"},
            "templating": {"list": variables}, "panels": panels, "links": links or []}

links = [{"title": "CI Releases", "type": "dashboards", "tags": ["gha-tracer"], "asDropdown": True}]

# ---------------- Release overview ----------------
_id = 0
L = 'resource.gha.release_line = "$line"'
RUN = 'span.gha.node_kind = "run"'
release = dash("gha-release-overview", "Release overview", [
    row("In progress now (live)", 0),
    infinity_table("Running for $line", "$tracer/api/active?release_line=$line", grid(0, 1, 24, 6),
                   "From gha-tracer memory. Empty after a restart until new events arrive."),
    row("Monthly release ($line.0 across repos)", 7),
    traceql_table("Release runs",
        '{ %s && resource.gha.change_type = "release" && %s } | select(resource.vcs.repository.name, resource.gha.version, span.gha.conclusion, span.gha.html_url)' % (L, RUN),
        grid(0, 8, 24, 9), "One row per repo run triggered by the vX.Y.0 tag. Click the trace ID for the waterfall."),
    row("Hotfixes on release/$line.x", 17),
    traceql_table("Hotfix runs (merge to release/* and patch tag, pair by SHA)",
        '{ %s && resource.gha.change_type =~ "hotfix-.*" && %s } | select(resource.vcs.repository.name, resource.gha.change_type, resource.gha.version, resource.gha.head_sha, span.gha.conclusion, span.gha.html_url)' % (L, RUN),
        grid(0, 18, 24, 10), "hotfix-merge rows have no version yet; the hotfix-tag row with the same head_sha names it."),
    row("What failed", 28),
    traceql_table("Failed jobs in $line",
        '{ %s && span.gha.node_kind = "job" && status = error } | select(resource.vcs.repository.name, resource.gha.change_type, resource.gha.version, span.gha.conclusion, span.gha.runner_label, span.gha.html_url)' % L,
        grid(0, 29, 12, 10)),
    traceql_table("Slowest jobs in $line (> 10m)",
        '{ %s && span.gha.node_kind = "job" && duration > 10m } | select(resource.vcs.repository.name, resource.gha.change_type, span.gha.runner_label)' % L,
        grid(12, 29, 12, 10)),
], [text_var("line", "Release line (vMAJOR.MINOR)", "v1.20"), const_var("tracer", "http://gha-tracer:9090")],
   "now-90d", ["gha-tracer"], links)

# ---------------- Live ----------------
_id = 0
live = dash("gha-live", "Release pipelines — live", [
    infinity_table("In progress (all release lines)", "$tracer/api/active?change_type=$type_live", grid(0, 0, 24, 10)),
    infinity_table("Completed since gha-tracer started (newest first)", "$tracer/api/recent?change_type=$type_live", grid(0, 10, 24, 12),
                   "In-memory, capped at RECENT_RUNS. Use Release overview for history."),
], [custom_var("type_live", "Type", ["release", "hotfix-merge", "hotfix-tag"], include_all=False) | {
        "includeAll": True, "allValue": "", "current": {"text": "All", "value": "$__all"}},
    const_var("tracer", "http://gha-tracer:9090")],
   "now-6h", ["gha-tracer"], links, refresh="30s")

# ---------------- Trends ----------------
_id = 0
T = 'change_type=~"$type"'
trends = dash("gha-trends", "Release pipeline trends", [
    stat("Runs in progress", 'sum(gha_runs_active) or vector(0)', grid(0, 0, 4, 4)),
    stat("Stuck runs", 'sum(gha_run_stale_active) or vector(0)', grid(4, 0, 4, 4),
         thresholds={"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "red", "value": 1}]}),
    stat("Failures (range)", 'sum(increase(gha_run_failures_total{%s}[$__range])) or vector(0)' % T, grid(8, 0, 4, 4),
         thresholds={"mode": "absolute", "steps": [{"color": "green", "value": None}, {"color": "red", "value": 1}]}),
    stat("Success rate (range)",
         'sum(increase(gha_runs_completed_total{%s,conclusion="success"}[$__range])) / sum(increase(gha_runs_completed_total{%s}[$__range]))' % (T, T),
         grid(12, 0, 4, 4), unit="percentunit"),
    stat("p95 run duration (range)",
         'histogram_quantile(0.95, sum by (le) (increase(gha_run_duration_seconds_bucket{%s}[$__range])))' % T, grid(16, 0, 4, 4), unit="s"),
    stat("p95 queue time (range)",
         'histogram_quantile(0.95, sum by (le) (increase(gha_job_queue_seconds_bucket{repo=~"$repo"}[$__range])))', grid(20, 0, 4, 4), unit="s"),
    row("Duration", 4),
    ts("p50 / p95 run duration by repo",
       [('histogram_quantile(0.95, sum by (le, repo) (rate(gha_run_duration_seconds_bucket{%s,repo=~"$repo"}[6h])))' % T, "p95 {{repo}}"),
        ('histogram_quantile(0.50, sum by (le, repo) (rate(gha_run_duration_seconds_bucket{%s,repo=~"$repo"}[6h])))' % T, "p50 {{repo}}")],
       grid(0, 5, 12, 9), unit="s"),
    ts("Release vs hotfix p95 duration",
       [('histogram_quantile(0.95, sum by (le, change_type) (rate(gha_run_duration_seconds_bucket{repo=~"$repo"}[1d])))', "{{change_type}}")],
       grid(12, 5, 12, 9), unit="s"),
    row("Reliability", 14),
    ts("Failures by repo",
       [('sum by (repo, change_type) (increase(gha_run_failures_total{%s,repo=~"$repo"}[1h]))' % T, "{{repo}} {{change_type}}")],
       grid(0, 15, 12, 8)),
    ts("Runs by conclusion",
       [('sum by (conclusion) (increase(gha_runs_completed_total{%s,repo=~"$repo"}[1h]))' % T, "{{conclusion}}")],
       grid(12, 15, 12, 8)),
    row("Jobs & runners", 23),
    bargauge("Slowest jobs (p95 over range)",
       'topk(15, histogram_quantile(0.95, sum by (le, repo, job) (increase(gha_job_duration_seconds_bucket{repo=~"$repo"}[$__range]))))',
       grid(0, 24, 12, 10)),
    ts("p95 queue time by runner label",
       [('histogram_quantile(0.95, sum by (le, runner_label) (rate(gha_job_queue_seconds_bucket{repo=~"$repo"}[1h])))', "{{runner_label}}")],
       grid(12, 24, 12, 10), unit="s"),
    row("gha-tracer health", 34),
    ts("Webhooks by result", [('sum by (type, result) (rate(gha_webhook_events_total[5m]))', "{{type}} {{result}}")], grid(0, 35, 8, 8), unit="reqps"),
    ts("GitHub API calls", [('sum by (endpoint, result) (rate(gha_github_api_calls_total[5m]))', "{{endpoint}} {{result}}")], grid(8, 35, 8, 8), unit="reqps"),
    ts("Errors", [('sum(rate(gha_emit_errors_total[5m]))', "export"),
                  ('sum by (stage) (rate(gha_process_errors_total[5m]))', "process {{stage}}"),
                  ('sum by (result) (rate(gha_redeliveries_total[5m]))', "redeliver {{result}}")], grid(16, 35, 8, 8)),
], [custom_var("type", "Type", ["release", "hotfix-merge", "hotfix-tag"]),
    {"name": "repo", "label": "Repo", "type": "query", "datasource": PROM, "query": {"query": "label_values(gha_runs_completed_total, repo)", "refId": "repo"},
     "includeAll": True, "allValue": ".*", "multi": True, "refresh": 2, "current": {"text": "All", "value": "$__all"}}],
   "now-30d", ["gha-tracer"], links)

os.makedirs(OUT, exist_ok=True)
for name, d in [("release-overview", release), ("live", live), ("trends", trends)]:
    with open(os.path.join(OUT, name + ".json"), "w") as f:
        json.dump(d, f, indent=2)
        f.write("\n")
    print("wrote", name)
