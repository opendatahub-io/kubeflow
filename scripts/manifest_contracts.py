#!/usr/bin/env python3
"""Manifest and build-input contract checks for the Konflux build simulator.

Every check here is aimed at a failure class that actually escaped in this
repository's history (Jira component "Notebooks Server"):

  copy-sources        a Dockerfile COPY whose source is missing from the
                      Konflux build context (path-context "components") makes
                      the PipelineRun image build fail after merge.
  rbac-watch-coverage RHOAIENG-100911: the manager watches Namespaces but the
                      shipped ClusterRole omits namespace list/watch, so a
                      fresh install never reaches DataScienceCluster Ready.
  rbac-identity       RHOAIENG-100514/-100515 class: a binding rendered or
                      applied without the ServiceAccount it names.
  webhook-contract    RHOAIENG-99636 (webhooks blocked by a default-deny
                      NetworkPolicy) and RHOAIENG-84738 (webhook-cert secret
                      missing): the webhook configuration, Service, container
                      port, NetworkPolicy and cert secret must agree.
  duplicate-env       RHOAIENG-57579 class: the API server keeps only the
                      last of two identically named env vars.
  manager-flags /
  help-flags          RHOAIENG-100779 class: an overlay passes a flag the
                      binary does not define and the container crash-loops
                      during flag parsing.
  image-params        RHOAIENG-55878 class: a rendered workload still points
                      at a placeholder or wrong image repository/tag, or a
                      params.env value never reaches the workload.
  prepare-kind-       Kind-phase helper: drop resources whose CRD does not
  manifests           exist in vanilla Kind and stop the rendered workload
                      from pulling the local-only PR tag from a registry.

Requires Python 3.9+ and PyYAML. Multi-document YAML inputs.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import sys

import yaml

WORKLOAD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "Job"}


def fail(msg: str) -> None:
    print(f"::error::{msg}", file=sys.stderr)
    sys.exit(1)


def load_docs(path: str):
    with open(path, encoding="utf-8") as handle:
        for doc in yaml.safe_load_all(handle):
            if doc is not None:
                yield doc


def pod_specs(docs):
    for doc in docs:
        kind = doc.get("kind")
        spec = doc.get("spec") or {}
        if kind in WORKLOAD_KINDS:
            yield doc, (spec.get("template") or {}).get("spec") or {}
        elif kind == "CronJob":
            job = (spec.get("jobTemplate") or {}).get("spec") or {}
            yield doc, (job.get("template") or {}).get("spec") or {}


def where(doc) -> str:
    meta = doc.get("metadata") or {}
    return f"{doc.get('kind')}/{meta.get('name')}"


def manager_containers(docs, container_name="manager"):
    """Every container named `manager` on any rendered workload.

    Keyed on the container name, not the workload name: the odh Deployment is
    called odh-notebook-controller-manager but the kf one is
    notebook-controller-deployment, and both run a container named manager.
    """
    found = []
    for doc, pod_spec in pod_specs(docs):
        for container in pod_spec.get("containers") or []:
            if container_name and container.get("name") != container_name:
                continue
            found.append((doc, container))
    return found


# --------------------------------------------------------------------------
# copy-sources: every Dockerfile COPY source must exist inside the context
# --------------------------------------------------------------------------

def cmd_copy_sources(args):
    with open(args.dockerfile, encoding="utf-8") as handle:
        lines = handle.readlines()

    problems = []
    for lineno, raw in enumerate(lines, start=1):
        line = raw.split("#", 1)[0].strip()
        if not line.startswith("COPY"):
            continue
        # Strip build-stage copies: their source lives in an earlier image,
        # not in the build context.
        if "--from=" in line:
            continue
        tokens = [t for t in shlex.split(line) if not t.startswith("--")]
        if len(tokens) < 3:
            if line.startswith("COPY ["):
                problems.append(f"{args.dockerfile}:{lineno}: JSON-form COPY not parsed: {line}")
            continue
        # tokens: COPY src... dest
        sources = tokens[1:-1]
        for source in sources:
            resolved = source.replace("${SOURCE_CODE}", ".")
            if os.path.isabs(resolved) or resolved.startswith(".."):
                problems.append(
                    f"{args.dockerfile}:{lineno}: COPY source '{source}' escapes the build context"
                )
                continue
            path = os.path.join(args.context_dir, resolved)
            if not os.path.exists(path):
                problems.append(
                    f"{args.dockerfile}:{lineno}: COPY source '{source}' not found at "
                    f"{os.path.relpath(path, os.getcwd())} (build context: {args.context_dir})"
                )
    if problems:
        print("❌ Dockerfile COPYs reference missing build-context paths:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print(f"✅ every COPY source exists inside the build context ({args.context_dir})")


# --------------------------------------------------------------------------
# rbac-watch-coverage
# --------------------------------------------------------------------------

# Go type -> (apiGroup, plural). Extend when a reconciler starts watching a
# new kind; an unknown type fails the check rather than being skipped.
WATCHED_KINDS = {
    "nbv1.Notebook": ("kubeflow.org", "notebooks"),
    "v1beta1.Notebook": ("kubeflow.org", "notebooks"),
    "corev1.ConfigMap": ("", "configmaps"),
    "corev1.Event": ("", "events"),
    "corev1.Namespace": ("", "namespaces"),
    "corev1.Pod": ("", "pods"),
    "corev1.Secret": ("", "secrets"),
    "corev1.Service": ("", "services"),
    "corev1.ServiceAccount": ("", "serviceaccounts"),
    "appsv1.StatefulSet": ("apps", "statefulsets"),
    "netv1.NetworkPolicy": ("networking.k8s.io", "networkpolicies"),
    "rbacv1.RoleBinding": ("rbac.authorization.k8s.io", "rolebindings"),
    "gatewayv1.HTTPRoute": ("gateway.networking.k8s.io", "httproutes"),
    "gatewayv1beta1.ReferenceGrant": ("gateway.networking.k8s.io", "referencegrants"),
}

# .For( and .Owns( reconcile the object (Get), .Watches( populates an
# informer. All three need get+list+watch. The builder chain is often wrapped
# across lines, so the dot and the method name may be separated by whitespace.
WATCH_CALL_RE = re.compile(
    r"\.\s*(For|Owns|Watches)\s*\(\s*(?:client\.Object\()?\s*&([A-Za-z0-9_]+\.[A-Za-z0-9_]+)\s*[,{)]"
)


def parse_watches(component_dir: str):
    """Return {(apiGroup, plural): set(call types)} for every kind the
    controller-builder watches in the component's Go sources."""
    watches = {}
    controllers_dir = os.path.join(component_dir, "controllers")
    if not os.path.isdir(controllers_dir):
        fail(f"{component_dir}: no controllers/ directory to parse")
    checked = 0
    for root, _, files in os.walk(controllers_dir):
        for name in sorted(files):
            if not name.endswith(".go") or name.endswith("_test.go"):
                continue
            checked += 1
            with open(os.path.join(root, name), encoding="utf-8") as handle:
                for match in WATCH_CALL_RE.finditer(handle.read()):
                    call, go_type = match.group(1), match.group(2)
                    gvk = WATCHED_KINDS.get(go_type)
                    if gvk is None:
                        fail(
                            f"unknown watched kind {go_type} in {name}: add it to "
                            f"WATCHED_KINDS in scripts/manifest_contracts.py"
                        )
                    watches.setdefault(gvk, set()).add(call)
    if not checked:
        fail(f"{controllers_dir}: no non-test Go sources found")
    return watches


def cluster_role_rules(docs):
    """Union of (apiGroup, resource) -> verbs over every ClusterRole."""
    rules = {}
    for doc in docs:
        if doc.get("kind") != "ClusterRole":
            continue
        for rule in doc.get("rules") or []:
            verbs = set(rule.get("verbs") or [])
            for group in rule.get("apiGroups") or [""]:
                for resource in rule.get("resources") or []:
                    rules.setdefault((group, resource), set()).update(verbs)
    return rules


def cmd_rbac_watch_coverage(args):
    watches = parse_watches(args.component_dir)
    if not watches:
        fail(f"{args.component_dir}: no For/Owns/Watches found — parser broken?")
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))
    rules = cluster_role_rules(docs)
    if not rules:
        fail("no ClusterRole found in the rendered manifests")

    missing = []
    for (group, plural), calls in sorted(watches.items()):
        verbs = rules.get((group, plural), set())
        if "*" in verbs:
            continue  # the wildcard grants every verb
        absent = {"list", "watch", "get"} - verbs
        if absent:
            missing.append(
                f"  {group or 'core'}/{plural} (watched via {', '.join(sorted(calls))}): "
                f"missing {', '.join(sorted(absent))}; granted: {', '.join(sorted(verbs)) or 'nothing'}"
            )
    if missing:
        print(
            "❌ manager watches kinds the shipped ClusterRole does not grant "
            "(RHOAIENG-100911: DSC never Ready):",
            file=sys.stderr,
        )
        print("\n".join(missing), file=sys.stderr)
        sys.exit(1)
    print(
        f"✅ every watched kind ({len(watches)}) has get/list/watch in the rendered ClusterRole"
    )


# --------------------------------------------------------------------------
# rbac-identity
# --------------------------------------------------------------------------

def cmd_rbac_identity(args):
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))
    service_accounts = set()
    for doc in docs:
        if doc.get("kind") != "ServiceAccount":
            continue
        meta = doc.get("metadata") or {}
        if meta.get("name"):
            service_accounts.add((meta.get("namespace") or "", meta["name"]))
    if not service_accounts:
        print("no ServiceAccount in render set; skipping identity check")
        return

    def resolves(namespace, name):
        return (namespace, name) in service_accounts or ("", name) in service_accounts

    problems = []
    for doc in docs:
        kind = doc.get("kind")
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or ""
        if kind in {"RoleBinding", "ClusterRoleBinding"}:
            for subject in doc.get("subjects") or []:
                if subject.get("kind") != "ServiceAccount":
                    continue
                name = subject.get("name")
                if kind == "ClusterRoleBinding":
                    if not subject.get("namespace"):
                        problems.append(
                            f"{where(doc)} binds {name} with no subject namespace"
                        )
                        continue
                    account_ns = subject["namespace"]
                else:
                    account_ns = subject.get("namespace") or namespace
                if not resolves(account_ns, name):
                    problems.append(f"{where(doc)} binds {account_ns}/{name}")

    for doc, pod_spec in pod_specs(docs):
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or ""
        sa = pod_spec.get("serviceAccountName")
        if sa and sa != "default" and not resolves(namespace, sa):
            problems.append(f"{where(doc)} runs as {namespace}/{sa}")

    if problems:
        print("❌ RBAC subjects without a matching rendered ServiceAccount:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print("✅ every RBAC subject and pod identity resolves to a rendered ServiceAccount")


# --------------------------------------------------------------------------
# webhook-contract
# --------------------------------------------------------------------------

def cmd_webhook_contract(args):
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))

    services = {}
    deployments = []
    webhook_configs = []
    for doc in docs:
        kind = doc.get("kind")
        meta = doc.get("metadata") or {}
        if kind == "Service":
            services[(meta.get("namespace") or "", meta["name"])] = doc
        elif kind == "Deployment":
            deployments.append(doc)
        elif kind in {"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"}:
            webhook_configs.append(doc)
    if not webhook_configs:
        print("no webhook configurations in render set; nothing to check")
        return
    if not deployments:
        fail("webhook configurations present but no Deployment in render set")

    problems = []

    # 1. Every webhook configuration's clientConfig service must resolve and
    #    expose the port the webhook server listens on.
    named_ports = set()
    port_numbers = {}
    for doc in deployments:
        for container in (doc["spec"]["template"]["spec"].get("containers") or []):
            for port in container.get("ports") or []:
                if port.get("name"):
                    named_ports.add(port["name"])
                    if isinstance(port.get("containerPort"), int):
                        port_numbers[port["name"]] = port["containerPort"]
    for doc in webhook_configs:
        for hook in doc.get("webhooks") or []:
            service = ((hook.get("clientConfig") or {}).get("service")) or {}
            service_port = service.get("port", 443)  # 443 when omitted
            key = (service.get("namespace"), service.get("name"))
            svc = services.get(key)
            if svc is None:
                problems.append(f"{where(doc)} webhook {hook.get('name')}: no Service {key}")
                continue
            ports = svc.get("spec", {}).get("ports") or []
            target = next(
                (p.get("targetPort") for p in ports if p.get("port") == service_port),
                None,
            )
            if target is None:
                problems.append(
                    f"Service/{key[1]} exposes no port {service_port} for webhook {hook.get('name')}"
                )
                continue
            if str(target) not in named_ports:
                problems.append(
                    f"Service/{key[1]} targetPort {target} matches no named container port "
                    f"({sorted(named_ports)}) on the controller Deployment"
                )

    # 2. The Service must request a serving cert and the Deployment must mount
    #    exactly that secret for the webhook cert dir (RHOAIENG-84738).
    managers = [d for d in deployments if "manager" in (d.get("metadata") or {}).get("name", "")]
    manager = managers[0] if managers else deployments[0]
    pod_spec = manager["spec"]["template"]["spec"]
    cert_secrets = set()
    for doc in docs:
        if doc.get("kind") != "Service":
            continue
        annotations = (doc.get("metadata") or {}).get("annotations") or {}
        secret_name = annotations.get("service.beta.openshift.io/serving-cert-secret-name")
        if secret_name:
            cert_secrets.add(secret_name)
    mounted = set()
    for volume in pod_spec.get("volumes") or []:
        secret = (volume.get("secret") or {}).get("secretName")
        if secret and "webhook" in volume.get("name", ""):
            mounted.add(secret)
    if not cert_secrets:
        problems.append(
            "no Service carries a serving-cert annotation but webhook configurations are shipped"
        )
    elif not mounted:
        problems.append("controller Deployment mounts no webhook cert secret volume")
    elif not (cert_secrets & mounted):
        problems.append(
            f"serving-cert secret {sorted(cert_secrets)} is not the mounted webhook secret {sorted(mounted)}"
        )

    # 3. A NetworkPolicy that selects the controller pod must allow the
    #    webhook port (RHOAIENG-99636: default-deny NP blocked the webhook).
    template_labels = (manager["spec"]["template"].get("metadata") or {}).get("labels") or {}
    webhook_port_numbers = {
        num for name, num in port_numbers.items() if name == "webhook"
    }
    for doc in docs:
        if doc.get("kind") != "NetworkPolicy":
            continue
        spec = doc.get("spec") or {}
        if "Ingress" not in (spec.get("policyTypes") or ["Ingress"]):
            continue
        selector = (spec.get("podSelector") or {}).get("matchLabels") or {}
        if not selector or not all(template_labels.get(k) == v for k, v in selector.items()):
            continue
        allowed = False
        for rule in spec.get("ingress") or []:
            if not rule.get("ports"):
                allowed = True  # a rule without port restriction allows everything
                break
            for port in rule.get("ports") or []:
                if port.get("port") in webhook_port_numbers and port.get("protocol", "TCP") == "TCP":
                    allowed = True
        if not allowed and webhook_port_numbers:
            problems.append(
                f"{where(doc)} selects the controller pod but allows no ingress on the "
                f"webhook port(s) {sorted(webhook_port_numbers)}"
            )

    if problems:
        print("❌ webhook contract violated:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print("✅ webhook configuration, Service, port, cert secret and NetworkPolicy agree")


# --------------------------------------------------------------------------
# duplicate-env
# --------------------------------------------------------------------------

def cmd_duplicate_env(args):
    problems = []
    for path in args.rendered:
        for doc, pod_spec in pod_specs(load_docs(path)):
            for container in (pod_spec.get("containers") or []) + (
                pod_spec.get("initContainers") or []
            ):
                names = [e["name"] for e in container.get("env") or [] if "name" in e]
                dups = sorted({n for n in names if names.count(n) > 1})
                if dups:
                    problems.append(
                        f"{where(doc)} container {container.get('name')}: duplicate env {dups}"
                    )
    if problems:
        print("❌ duplicate env var names (API server keeps only the last):", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print("✅ no duplicate env vars on rendered workloads")


# --------------------------------------------------------------------------
# manager-flags / help-flags
# --------------------------------------------------------------------------

def normalize_flag(token: str) -> str:
    token = token.strip()
    if not token.startswith("-"):
        return ""
    name = token.lstrip("-").split("=", 1)[0]
    return f"--{name}" if name else ""


def cmd_manager_flags(args):
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))
    containers = manager_containers(docs, args.container)
    if not containers:
        fail("no container named manager found in rendered workloads")
    flags = set()
    for _, container in containers:
        argv = list(container.get("command") or []) + list(container.get("args") or [])
        for token in argv:
            flag = normalize_flag(str(token))
            if flag:
                flags.add(flag)
    if not flags:
        # A manager that runs on defaults alone (the kf controller) defines no
        # flags; print a marker so the caller can tell "no flags" apart from
        # "nothing found".
        print("## no flags")
        return
    for flag in sorted(flags):
        print(flag)


HELP_FLAG_RE = re.compile(r"^\s+-{1,2}([A-Za-z0-9][A-Za-z0-9_.-]*)")


def cmd_help_flags(args):
    flags = set()
    with open(args.help_file, encoding="utf-8") as handle:
        for line in handle:
            match = HELP_FLAG_RE.match(line)
            if match:
                flags.add(f"--{match.group(1)}")
    if not flags:
        fail(f"{args.help_file}: no flags parsed from --help output")
    for flag in sorted(flags):
        print(flag)


# --------------------------------------------------------------------------
# image-params: params.env values must reach the rendered workload
# --------------------------------------------------------------------------

def load_env_file(path: str):
    params = {}
    with open(path, encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            params[key.strip()] = value.strip()
    return params


def cmd_image_params(args):
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))
    containers = manager_containers(docs)
    if not containers:
        fail("no manager workload found in rendered manifests")

    problems = []

    for pair in args.image_param or []:
        name, _, path = pair.partition("=")
        expected = load_env_file(path).get(name)
        if expected is None:
            fail(f"{pair}: key {name} not present in {path}")
        if not any(c.get("image") == expected for _, c in containers):
            got = ", ".join(sorted({str(c.get("image")) for _, c in containers}))
            problems.append(f"{name}: no manager container image equals '{expected}' (images: {got})")

    for pair in args.arg_param or []:
        name, _, path = pair.partition("=")
        expected = load_env_file(path).get(name)
        if expected is None:
            fail(f"{pair}: key {name} not present in {path}")
        if not any(
            any(str(t).endswith(expected) for t in list(c.get("args") or []) + list(c.get("command") or []))
            for _, c in containers
        ):
            problems.append(f"{name}: no manager container argument ends with '{expected}'")

    for pair in args.env_param or []:
        name, _, rest = pair.partition("=")
        env_name, _, path = rest.partition(":")
        expected = load_env_file(path).get(name)
        if expected is None:
            fail(f"{pair}: key {name} not present in {path}")
        seen = False
        match = False
        for _, c in containers:
            for entry in c.get("env") or []:
                if entry.get("name") == env_name:
                    seen = True
                    if entry.get("value") == expected:
                        match = True
        if seen and not match:
            problems.append(f"{name}: manager env {env_name} exists but does not equal '{expected}'")
        elif not seen:
            problems.append(f"{name}: manager env {env_name} not present in rendered workload")

    if problems:
        print("❌ params.env values not honoured by the rendered workload:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        sys.exit(1)
    print("✅ rendered workloads honour every checked params.env value")


# --------------------------------------------------------------------------
# config-refs: workload ConfigMap/Secret references must resolve
# --------------------------------------------------------------------------

def cmd_config_refs(args):
    """Every non-optional ConfigMap/Secret a rendered workload references must
    either be rendered alongside it or be an explicitly allowed external
    dependency (platform- or sibling-component-provided)."""
    allow = set(args.allow or [])
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))

    rendered = {"ConfigMap": set(), "Secret": set()}
    for doc in docs:
        kind = doc.get("kind")
        if kind in rendered:
            rendered[kind].add((doc.get("metadata") or {}).get("name"))

    def resolves(kind, name):
        return name in rendered[kind] or name in allow

    problems = []
    for doc, pod_spec in pod_specs(docs):
        for container in (pod_spec.get("containers") or []) + (
            pod_spec.get("initContainers") or []
        ):
            for entry in container.get("env") or []:
                source = entry.get("valueFrom") or {}
                ref = source.get("configMapKeyRef")
                if ref and not ref.get("optional") and not resolves("ConfigMap", ref.get("name")):
                    problems.append(
                        f"{where(doc)} container {container.get('name')}: env {entry.get('name')} "
                        f"references ConfigMap {ref.get('name')} which is not rendered"
                    )
                ref = source.get("secretRef")
                if ref and not ref.get("optional") and not resolves("Secret", ref.get("name")):
                    problems.append(
                        f"{where(doc)} container {container.get('name')}: env {entry.get('name')} "
                        f"references Secret {ref.get('name')} which is not rendered"
                    )
        for volume in pod_spec.get("volumes") or []:
            name = (volume.get("configMap") or {}).get("name")
            if name and not resolves("ConfigMap", name):
                problems.append(
                    f"{where(doc)}: volume {volume.get('name')} references ConfigMap {name} "
                    f"which is not rendered"
                )
            name = (volume.get("secret") or {}).get("secretName")
            if name and not (volume.get("secret") or {}).get("optional") and not resolves("Secret", name):
                problems.append(
                    f"{where(doc)}: volume {volume.get('name')} references Secret {name} "
                    f"which is not rendered"
                )

    if problems:
        print("❌ workload references objects that nothing renders (stuck containers):", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        if allow:
            print(f"  (allowed externals: {', '.join(sorted(allow))})", file=sys.stderr)
        sys.exit(1)
    print("✅ every non-optional ConfigMap/Secret reference resolves (or is an allowed external)")


# --------------------------------------------------------------------------
# prepare-kind-manifests
# --------------------------------------------------------------------------

def cmd_prepare_kind_manifests(args):
    """Drop resources whose CRD does not exist in vanilla Kind and stop the
    rendered workload from pulling the local-only PR tag from a registry."""
    kept = []
    dropped = []
    for doc in load_docs(args.infile):
        kind = doc.get("kind") or ""
        if kind in args.drop_kinds:
            dropped.append(where(doc))
            continue
        if kind in WORKLOAD_KINDS:
            spec = doc.get("spec") or {}
            pod_spec = (spec.get("template") or {}).get("spec") or {}
            for container in (pod_spec.get("containers") or []) + (
                pod_spec.get("initContainers") or []
            ):
                if container.get("imagePullPolicy") == "Always":
                    container["imagePullPolicy"] = "IfNotPresent"
        kept.append(doc)
    with open(args.outfile, "w", encoding="utf-8") as handle:
        yaml.safe_dump_all(kept, handle, explicit_start=True)
    for name in dropped:
        print(f"dropped {name} (no CRD in vanilla Kind)")
    print(f"✅ prepared {len(kept)} documents for Kind ({args.outfile})")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("copy-sources")
    p.add_argument("dockerfile")
    p.add_argument("context_dir")
    p.set_defaults(func=cmd_copy_sources)

    p = sub.add_parser("rbac-watch-coverage")
    p.add_argument("component_dir", help="component directory with controllers/*.go")
    p.add_argument("rendered", nargs="+", help="rendered manifests applied together")
    p.set_defaults(func=cmd_rbac_watch_coverage)

    p = sub.add_parser("rbac-identity")
    p.add_argument("rendered", nargs="+")
    p.set_defaults(func=cmd_rbac_identity)

    p = sub.add_parser("webhook-contract")
    p.add_argument("rendered", nargs="+")
    p.set_defaults(func=cmd_webhook_contract)

    p = sub.add_parser("duplicate-env")
    p.add_argument("rendered", nargs="+")
    p.set_defaults(func=cmd_duplicate_env)

    p = sub.add_parser("config-refs")
    p.add_argument("rendered", nargs="+")
    p.add_argument(
        "--allow",
        action="append",
        default=[],
        help="name of an externally-provided ConfigMap/Secret (repeatable)",
    )
    p.set_defaults(func=cmd_config_refs)

    p = sub.add_parser("manager-flags")
    p.add_argument("rendered", nargs="+")
    p.add_argument("--container", default="manager")
    p.set_defaults(func=cmd_manager_flags)

    p = sub.add_parser("help-flags")
    p.add_argument("help_file")
    p.set_defaults(func=cmd_help_flags)

    p = sub.add_parser("image-params")
    p.add_argument("rendered", nargs="+")
    p.add_argument("--image-param", action="append", help="NAME=params.env")
    p.add_argument("--arg-param", action="append", help="NAME=params.env")
    p.add_argument("--env-param", action="append", help="NAME=ENV_VAR:params.env")
    p.set_defaults(func=cmd_image_params)

    p = sub.add_parser("prepare-kind-manifests")
    p.add_argument("infile")
    p.add_argument("outfile")
    p.add_argument(
        "--drop-kind", action="append", default=[], dest="drop_kinds", help="kind to drop (repeatable)"
    )
    p.set_defaults(func=cmd_prepare_kind_manifests)

    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
