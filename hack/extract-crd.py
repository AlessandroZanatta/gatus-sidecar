#!/usr/bin/env python3
"""Extract one CRD from a multi-document manifest on stdin.

Used to vendor the Traefik IngressRoute CRD for the e2e tests. The real schema
is kept rather than a permissive stub, so the fixtures are validated the way the
cluster would validate them.

Usage: extract-crd.py <crd-name> <source-version> < bundle.yaml > out.yaml
"""

import sys

import yaml


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2

    name, version = sys.argv[1], sys.argv[2]

    docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
    matching = [d for d in docs if d.get("metadata", {}).get("name") == name]
    if not matching:
        print(f"no CRD named {name} in the input", file=sys.stderr)
        return 1

    sys.stdout.write(
        f"# The official Traefik {matching[0]['spec']['names']['kind']} CRD, vendored from\n"
        f"# traefik/traefik {version} integration/fixtures/k8s/01-traefik-crd.yml.\n"
        "#\n"
        "# The real schema is used rather than a permissive stub so the fixtures in\n"
        "# these tests are validated the way the cluster would validate them: a\n"
        "# manifest the sidecar accepts but Traefik would reject is a bug worth\n"
        "# catching here.\n"
        "#\n"
        "# Refresh with: make update-traefik-crd\n"
    )
    yaml.safe_dump(matching[0], sys.stdout, sort_keys=False, default_flow_style=False, width=100)
    return 0


if __name__ == "__main__":
    sys.exit(main())
