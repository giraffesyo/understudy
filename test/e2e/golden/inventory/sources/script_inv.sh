#!/bin/sh
# An inventory script: the script plugin runs it with --list.
if [ "$1" = "--list" ]; then
  cat <<'EOF'
{
  "web": {"hosts": ["s1", "s2"], "vars": {"tier": "front"}, "children": ["edge"]},
  "edge": ["s3"],
  "_meta": {"hostvars": {"s1": {"idx": 1}, "s3": {"idx": 3, "labels": ["a", "b"]}}}
}
EOF
else
  echo '{}'
fi
