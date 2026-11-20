#!/usr/bin/env python3
"""Generates deploy/grafana/dashboards/tag-runs.json.
Edit here, run `python3 deploy/grafana/gen_dashboards.py`, commit the output.

One dashboard: every workflow run triggered by a tag push, across all repos,
for the selected time range, with its final (or current) status.

Query shape (all instant queries over $__range):
  * last_over_time(...[$__range]) finds every run exported at any point in the
    range, even though gha-tracer stops exporting a run ~1h after it finishes.
  * topk by (repo, run_id) on gha_tag_run_updated_seconds picks the freshest
    copy of a run if it was exported by two gha-tracer tasks (restart/deploy
    changes the `instance` label).
  * started >= $__from keeps only runs triggered inside the range.
"""
import json, os

OUT = os.path.join(os.path.dirname(__file__), "dashboards")
PROM = {"type": "prometheus", "uid": "prometheus"}

SEL = '{repo=~"$repo"}'
RANGE = "[$__range]"


def lot(metric):
    return f"last_over_time({metric}{SEL}{RANGE})"


LATEST = f"topk by (repo, run_id) (1, {lot('gha_tag_run_updated_seconds')})"
IN_RANGE = f"({lot('gha_tag_run_started_seconds')} >= $__from / 1000)"


def pick(metric):
    return f"{lot(metric)} and on (repo, run_id, instance) {LATEST} and on (repo, run_id) {IN_RANGE}"


def status(filter_):
    return f"({pick('gha_tag_run_status')}) {filter_}"


STATUS_F = status("$status")

# The outer max by (...) is an identity (one series per group) that gives all
# three table queries the same label set, with no __name__ or job, so
# Grafana's Merge transformation lines their rows up.
KEEP = "repo, workflow, tag, run_id, instance"


def row(expr):
    return f"max by ({KEEP}) ({expr})"


STATUS_Q = row(STATUS_F)
STARTED_Q = row(f"({pick('gha_tag_run_started_seconds')}) * 1000 and on (repo, run_id, instance) ({STATUS_F})")
DURATION_Q = row(f"{pick('gha_tag_run_duration_seconds')} and on (repo, run_id, instance) ({STATUS_F})")

STATUSES = [  # value, text, Grafana color
    (0, "Running", "blue"),
    (1, "Success", "green"),
    (2, "Failed", "red"),
    (3, "Cancelled", "orange"),
    (4, "Timed out", "red"),
    (5, "Other", "text"),
]


def target(ref, expr):
    return {"refId": ref, "datasource": PROM, "expr": expr, "instant": True, "range": False,
            "format": "table", "legendFormat": ""}


def stat(pid, title, expr, color, x):
    return {
        "id": pid, "type": "stat", "title": title, "datasource": PROM,
        "gridPos": {"x": x, "y": 0, "w": 6, "h": 4},
        "targets": [{"refId": "A", "datasource": PROM, "expr": expr, "instant": True, "range": False}],
        "fieldConfig": {"defaults": {"color": {"mode": "fixed", "fixedColor": color}, "unit": "none",
                                     "decimals": 0}, "overrides": []},
        "options": {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "", "values": False},
                    "colorMode": "value", "graphMode": "none", "textMode": "value", "justifyMode": "center"},
    }


def by_name(name, *props):
    return {"matcher": {"id": "byName", "options": name}, "properties": list(props)}


table = {
    "id": 5, "type": "table", "title": "Workflows triggered by a tag push",
    "description": "One row per run (re-runs show their latest attempt). Status of a workflow includes every "
                   "reusable workflow it calls, so a failing child shows here as Failed.",
    "datasource": PROM, "gridPos": {"x": 0, "y": 4, "w": 24, "h": 20},
    "targets": [target("A", STATUS_Q), target("B", STARTED_Q), target("C", DURATION_Q)],
    "transformations": [
        {"id": "merge", "options": {}},
        {"id": "organize", "options": {
            "excludeByName": {"Time": True, "instance": True},
            "indexByName": {"Value #B": 0, "repo": 1, "workflow": 2, "tag": 3, "Value #A": 4, "Value #C": 5, "run_id": 6},
            "renameByName": {"Value #B": "Started", "repo": "Repo", "workflow": "Workflow", "tag": "Tag",
                             "Value #A": "Status", "Value #C": "Duration", "run_id": "Run"},
        }},
    ],
    "fieldConfig": {
        "defaults": {"custom": {"align": "auto", "cellOptions": {"type": "auto"}}},
        "overrides": [
            by_name("Started", {"id": "unit", "value": "dateTimeAsLocal"}, {"id": "custom.width", "value": 190}),
            by_name("Status",
                    {"id": "mappings", "value": [{"type": "value", "options": {
                        str(v): {"text": t, "color": c, "index": i} for i, (v, t, c) in enumerate(STATUSES)}}]},
                    {"id": "custom.cellOptions", "value": {"type": "color-background", "mode": "basic"}},
                    {"id": "custom.width", "value": 130}),
            by_name("Duration", {"id": "unit", "value": "s"}, {"id": "decimals", "value": 0},
                    {"id": "custom.width", "value": 120}),
            by_name("Run", {"id": "links", "value": [{
                "title": "Open run in GitHub", "targetBlank": True,
                "url": "${github_url}/${__data.fields.Repo}/actions/runs/${__value.raw}"}]},
                    {"id": "custom.width", "value": 140}),
        ],
    },
    "options": {"showHeader": True, "cellHeight": "sm", "sortBy": [{"displayName": "Started", "desc": True}],
                "footer": {"show": False}},
}

panels = [
    stat(1, "Tag runs", f"count({status('>= 0')}) or vector(0)", "text", 0),
    stat(2, "Succeeded", f"count({status('== 1')}) or vector(0)", "green", 6),
    stat(3, "Failed / cancelled", f"count({status('>= 2 < 5')}) or vector(0)", "red", 12),
    stat(4, "Running", f"count({status('== 0')}) or vector(0)", "blue", 18),
    table,
]

status_opts = [("All", ">= 0")] + [(t, f"== {v}") for v, t, _ in STATUSES if v < 5]

dashboard = {
    "uid": "gha-tag-runs",
    "title": "Tag-triggered workflows",
    "tags": ["ci", "github-actions"],
    "timezone": "browser",
    "editable": True,
    "schemaVersion": 41,
    "time": {"from": "now-6h", "to": "now"},
    "refresh": "30s",
    "templating": {"list": [
        {"name": "repo", "label": "repo", "type": "query", "datasource": PROM,
         "query": {"qryType": 1, "query": "label_values(gha_tag_run_status, repo)",
                   "refId": "PrometheusVariableQueryEditor-VariableQuery"},
         "definition": "label_values(gha_tag_run_status, repo)",
         "refresh": 2, "includeAll": True, "multi": True, "allValue": ".*", "sort": 1,
         "current": {"text": ["All"], "value": ["$__all"]}},
        {"name": "status", "label": "status", "type": "custom",
         "query": ",".join(f"{t} : {v}" for t, v in status_opts),
         "options": [{"text": t, "value": v, "selected": i == 0} for i, (t, v) in enumerate(status_opts)],
         "current": {"text": "All", "value": ">= 0"}, "includeAll": False, "multi": False},
        {"name": "github_url", "type": "constant", "hide": 2, "query": "https://github.com",
         "current": {"text": "https://github.com", "value": "https://github.com"}},
    ]},
    "panels": panels,
}

os.makedirs(OUT, exist_ok=True)
with open(os.path.join(OUT, "tag-runs.json"), "w") as f:
    json.dump(dashboard, f, indent=2)
    f.write("\n")
print("wrote", os.path.join(OUT, "tag-runs.json"))
