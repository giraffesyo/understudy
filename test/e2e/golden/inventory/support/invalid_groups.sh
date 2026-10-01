#!/bin/sh
cat <<'JSON'
{"web-servers": {"hosts": ["h1"], "vars": {"x": 1}, "children": ["db.prod"]},
 "db.prod": {"hosts": ["h2"]},
 "_meta": {"hostvars": {"h1": {"ansible_connection": "local"}, "h2": {"ansible_connection": "local"}}}}
JSON
