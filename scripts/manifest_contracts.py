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

# A watch whose object argument is a bare identifier (a variable assigned
# elsewhere, e.g. `ns := &corev1.Namespace{}; ... Watches(ns, ...)`) is
# resolved through a per-file assignment scan; a variable we cannot resolve
# fails loudly rather than silently under-checking.
WATCH_CALL_BARE_RE = re.compile(
    r"\.\s*(For|Owns|Watches)\s*\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*[,)]"
)
TYPED_ASSIGN_RE = re.compile(
    r"([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*&([A-Za-z0-9_]+\.[A-Za-z0-9_]+)\s*\{"
)
UNSTRUCTURED_ASSIGN_RE = re.compile(
    r"([A-Za-z_][A-Za-z0-9_]*)\s*:?=\s*&unstructured\.Unstructured\s*\{"
)


def pluralize(kind: str) -> str:
    """Kind -> resource plural, matching what the API server serves."""
    lowered = kind.lower()
    if lowered.endswith(("s", "x", "ch", "sh")):
        return lowered + "es"
    if lowered.endswith("y"):
        return lowered[:-1] + "ies"
    return lowered + "s"


def watch_variable_types(source: str, name: str, filename: str):
    """Map a variable name to the (apiGroup, plural) it watches.

    Recognizes `v := &pkg.Type{}` and the dynamic-client idiom
    `v := &unstructured.Unstructured{}` followed by SetAPIVersion/SetKind.
    If the name is assigned more than one distinct type in the file, the
    call-site binding cannot be determined syntactically: fail loudly
    instead of guessing one of them."""
    resolved = set()
    for match in TYPED_ASSIGN_RE.finditer(source):
        if match.group(1) != name:
            continue
        if match.group(2) == "unstructured.Unstructured":
            continue  # handled by the dynamic-client pattern below
        gvk = WATCHED_KINDS.get(match.group(2))
        if gvk is None:
            fail(
                f"variable {name} is assigned unknown watched kind {match.group(2)}: "
                f"add it to WATCHED_KINDS in scripts/manifest_contracts.py"
            )
        resolved.add(gvk)
    for match in UNSTRUCTURED_ASSIGN_RE.finditer(source):
        if match.group(1) != name:
            continue
        window = source[match.end():match.end() + 600]
        api_version = re.search(
            rf"{name}\.SetAPIVersion\(\"([^\"]+)\"\)", window
        )
        kind = re.search(rf"{name}\.SetKind\(\"([^\"]+)\"\)", window)
        if api_version and kind:
            group = api_version.group(1).partition("/")[0]
            resolved.add((group, pluralize(kind.group(1))))
    if len(resolved) > 1:
        fail(
            f"{filename}: variable '{name}' is assigned {len(resolved)} distinct types "
            f"({sorted(resolved)}); the call-site binding cannot be resolved "
            f"syntactically. Pass the type literal to the watch call instead."
        )
    return next(iter(resolved)) if resolved else None


def parse_watches(component_dir: str):
    """Return {(apiGroup, plural): set(call types)} for every kind the
    controller-builder watches in the component's Go sources."""
    watches = {}
    controllers_dir = os.path.join(component_dir, "controllers")
    if not os.path.isdir(controllers_dir):
        fail(f"{component_dir}: no controllers/ directory to parse")
    checked = 0
    tree_root = os.path.realpath(controllers_dir)
    for root, _, files in os.walk(controllers_dir):
        for name in sorted(files):
            if not name.endswith(".go") or name.endswith("_test.go"):
                continue
            path = os.path.join(root, name)
            # A symlinked .go file could point outside the controller tree;
            # only read sources that stay inside it.
            if os.path.realpath(path) != path and not os.path.realpath(path).startswith(
                tree_root + os.sep
            ):
                fail(f"{name}: symlink escapes the controller tree; refusing to read it")
            checked += 1
            source = open(path, encoding="utf-8").read()
            for match in WATCH_CALL_BARE_RE.finditer(source):
                if WATCH_CALL_RE.search(match.group(0)):
                    continue
                call, variable = match.group(1), match.group(2)
                gvk = watch_variable_types(source, variable, name)
                if gvk is None:
                    fail(
                        f"{name}: {call}() is passed the variable '{variable}' whose type "
                        f"this parser cannot resolve. Pass the type literal, or teach "
                        f"watch_variable_types in scripts/manifest_contracts.py to resolve it."
                    )
                watches.setdefault(gvk, set()).add(call)
            for match in WATCH_CALL_RE.finditer(source):
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


def workload_service_accounts(docs):
    """{(namespace, name)} for every ServiceAccount a rendered pod runs as.

    A workload that omits serviceAccountName runs as the implicit `default`
    ServiceAccount of its own namespace, so that pair is recorded too."""
    accounts = set()
    for doc, pod_spec in pod_specs(docs):
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or "default"
        sa = pod_spec.get("serviceAccountName") or pod_spec.get("serviceAccount") or "default"
        accounts.add((namespace, sa))
    return accounts


def manager_service_accounts(docs):
    """{(namespace, name)} for the controller manager workload only.

    Grants held by *other* rendered workloads (e.g. a culler or proxy) must
    not satisfy the manager's watch coverage."""
    workloads = [w for w in pod_specs(docs)]
    if not workloads:
        return set()
    managers = [w for w in workloads if "manager" in (w[0].get("metadata") or {}).get("name", "")]
    chosen = managers or [workloads[0]]
    accounts = set()
    for doc, pod_spec in chosen:
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or "default"
        sa = pod_spec.get("serviceAccountName") or pod_spec.get("serviceAccount") or "default"
        accounts.add((namespace, sa))
    return accounts


def bound_role_rules(docs, accounts=None):
    """Union of (apiGroup, resource) -> verbs over exactly the roles that
    reach the given ServiceAccounts (default: every rendered workload's): a
    role counts only when a binding whose subject is one of those accounts
    references it. An unbound (or differently bound) ClusterRole in the
    render grants the controller nothing.

    A ClusterRole referenced through a *RoleBinding* is deliberately not
    counted: that grants access only within the binding's namespace, which
    cannot back a cluster-wide list/watch informer. Rules restricted by
    `resourceNames` are excluded for the same reason."""
    if accounts is None:
        accounts = workload_service_accounts(docs)
    wanted = {"ClusterRole": set(), "Role": set()}  # Role is namespaced
    for doc in docs:
        kind = doc.get("kind")
        if kind not in {"RoleBinding", "ClusterRoleBinding"}:
            continue
        meta = doc.get("metadata") or {}
        binding_ns = meta.get("namespace") or "default"
        for subject in doc.get("subjects") or []:
            if subject.get("kind") != "ServiceAccount":
                continue
            subject_ns = subject.get("namespace") or (
                binding_ns if kind == "RoleBinding" else ""
            )
            if (subject_ns, subject.get("name")) not in accounts:
                continue
            ref = doc.get("roleRef") or {}
            ref_kind = ref.get("kind")
            if ref_kind == "ClusterRole" and kind == "ClusterRoleBinding":
                wanted["ClusterRole"].add(ref.get("name"))
            elif ref_kind == "Role":
                wanted["Role"].add((binding_ns, ref.get("name")))
    rules = {}
    for doc in docs:
        kind = doc.get("kind")
        if kind not in ("ClusterRole", "Role"):
            continue
        meta = doc.get("metadata") or {}
        if kind == "Role":
            if (meta.get("namespace") or "default", meta.get("name")) not in wanted["Role"]:
                continue
        elif meta.get("name") not in wanted["ClusterRole"]:
            continue
        for rule in doc.get("rules") or []:
            if rule.get("resourceNames"):
                continue  # name-restricted grants cannot serve an informer
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
    # Only grants that reach the *manager* count: a verb held by another
    # rendered workload's ServiceAccount (culler, proxy, ...) does not
    # authorize the manager's informers.
    rules = bound_role_rules(docs, accounts=manager_service_accounts(docs))
    if not rules:
        fail("no role bound to the manager's ServiceAccount found")

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
        # An empty set is still a meaningful check: every subject and pod
        # identity must then fail to resolve (unless it uses the implicit
        # `default` ServiceAccount, which the API server always provides).
        print("no ServiceAccount in render set; checking references against the empty set")

    def resolves(namespace, name):
        if name == "default":
            return True  # implicit in every namespace
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

def selector_selects(pod_selector, labels):
    """True when a LabelSelector (matchLabels + matchExpressions) selects a pod
    template with the given labels. An empty selector selects everything."""
    pod_selector = pod_selector or {}
    match_labels = pod_selector.get("matchLabels") or {}
    if any(labels.get(k) != v for k, v in match_labels.items()):
        return False
    for expr in pod_selector.get("matchExpressions") or []:
        key, op, values = expr.get("key"), expr.get("operator"), expr.get("values") or []
        present = key in labels
        if op == "In" and (not present or labels[key] not in values):
            return False
        if op == "NotIn" and present and labels[key] in values:
            return False
        if op == "Exists" and not present:
            return False
        if op == "DoesNotExist" and present:
            return False
    return True


def cmd_webhook_contract(args):
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))

    services = {}
    workloads = []  # (doc, namespace, template_labels, pod_spec)
    webhook_configs = []
    for doc in docs:
        kind = doc.get("kind")
        meta = doc.get("metadata") or {}
        if kind == "Service":
            services[(meta.get("namespace") or "default", meta["name"])] = doc
        elif kind in WORKLOAD_KINDS:
            template = ((doc.get("spec") or {}).get("template") or {})
            workloads.append(
                (
                    doc,
                    meta.get("namespace") or "default",
                    (template.get("metadata") or {}).get("labels") or {},
                    template.get("spec") or {},
                )
            )
        elif kind in {"MutatingWebhookConfiguration", "ValidatingWebhookConfiguration"}:
            webhook_configs.append(doc)
    if not webhook_configs:
        print("no webhook configurations in render set; nothing to check")
        return
    if not workloads:
        fail("webhook configurations present but no workload in render set")

    problems = []
    # Container ports webhook traffic actually lands on (Service port ->
    # targetPort -> backend container port), and the Services referenced by
    # webhook configurations.
    resolved_webhook_ports = set()
    referenced_services = {}

    # 1. Every webhook configuration's clientConfig service must resolve, its
    #    selector must match a rendered workload, and that backend must listen
    #    on the Service's targetPort (checking a port on an unrelated
    #    Deployment would prove nothing).
    for doc in webhook_configs:
        for hook in doc.get("webhooks") or []:
            service = ((hook.get("clientConfig") or {}).get("service")) or {}
            service_port = service.get("port", 443)  # 443 when omitted
            key = (service.get("namespace") or "default", service.get("name"))
            svc = services.get(key)
            if svc is None:
                problems.append(f"{where(doc)} webhook {hook.get('name')}: no Service {key}")
                continue
            referenced_services[key] = svc
            selector = (svc.get("spec") or {}).get("selector") or {}
            if not selector:
                problems.append(
                    f"Service/{key[1]} has no selector; nothing routes the webhook traffic"
                )
                continue
            backends = [
                w for w in workloads
                if w[1] == key[0] and all(w[2].get(k) == v for k, v in selector.items())
            ]
            if not backends:
                problems.append(
                    f"Service/{key[1]} selector {selector} matches no rendered workload"
                )
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
            listens = [
                port.get("containerPort")
                for _, _, _, pod_spec in backends
                for container in pod_spec.get("containers") or []
                for port in container.get("ports") or []
                if (port.get("name") == str(target) or port.get("containerPort") == target)
                and isinstance(port.get("containerPort"), int)
            ]
            if not listens:
                problems.append(
                    f"Service/{key[1]} targetPort {target} matches no container port on the "
                    f"workload its selector selects"
                )
            else:
                resolved_webhook_ports.update(listens)

    # 2. Every Service referenced by a webhook must request a serving cert, and
    #    the manager container must mount that exact secret (RHOAIENG-84738).
    #    Follow the container's volumeMounts, not the pod's volumes: a volume
    #    without a mount serves nobody. Each referenced Service is validated
    #    on its own: one correctly annotated Service must not cover for
    #    another that lost its annotation.
    managers = [w for w in workloads if "manager" in (w[0].get("metadata") or {}).get("name", "")]
    manager = managers[0] if managers else workloads[0]
    doc, manager_ns, template_labels, pod_spec = manager
    containers = pod_spec.get("containers") or []
    manager_container = next(
        (c for c in containers if c.get("name") == "manager"), containers[0] if containers else None
    )
    volumes_by_name = {v.get("name"): v for v in pod_spec.get("volumes") or []}
    mounted_secrets = set()
    if manager_container:
        for mount in manager_container.get("volumeMounts") or []:
            volume = volumes_by_name.get(mount.get("name")) or {}
            secret = (volume.get("secret") or {}).get("secretName")
            if secret:
                mounted_secrets.add(secret)
    if not mounted_secrets and referenced_services:
        problems.append("manager container mounts no secret volumes at all")
    for (svc_ns, svc_name), svc in sorted(referenced_services.items()):
        annotations = (svc.get("metadata") or {}).get("annotations") or {}
        secret_name = annotations.get("service.beta.openshift.io/serving-cert-secret-name")
        if not secret_name:
            problems.append(
                f"Service/{svc_name} is referenced by a webhook but carries no "
                f"serving-cert annotation"
            )
        elif secret_name not in mounted_secrets:
            problems.append(
                f"serving-cert secret {secret_name} requested by Service/{svc_name} is not "
                f"mounted by the manager container (mounted: {sorted(mounted_secrets)})"
            )

    # 3. A NetworkPolicy that selects the controller pod must allow the
    #    webhook port (RHOAIENG-99636: default-deny NP blocked the webhook).
    #    * The policy only applies in its own namespace.
    #    * An empty podSelector selects every pod in that namespace;
    #      matchExpressions are evaluated against the manager's labels.
    #    * The port that must be allowed is the one the webhook Service
    #      actually targets (resolved in section 1), not any port the manager
    #      happens to declare.
    #    * A port rule with `from`/`to` peers cannot be relied on for
    #      kube-apiserver traffic (it originates outside any selectable pod
    #      network), so only unrestricted-source port rules count.
    webhook_ports = resolved_webhook_ports
    if not webhook_ports:
        # No service-based webhook resolved its backend port (section 1
        # already reports that). Fall back to ports the manager declares for
        # the webhook so the NetworkPolicy guard still applies.
        for container in containers:
            for port in container.get("ports") or []:
                if port.get("name") == "webhook" and isinstance(port.get("containerPort"), int):
                    webhook_ports.add(port["containerPort"])
        if not webhook_ports and not problems:
            problems.append("manager container declares no port named webhook")
    for doc2 in docs:
        if doc2.get("kind") != "NetworkPolicy":
            continue
        spec = doc2.get("spec") or {}
        if "Ingress" not in (spec.get("policyTypes") or ["Ingress"]):
            continue
        meta = doc2.get("metadata") or {}
        policy_ns = meta.get("namespace") or "default"
        if policy_ns != manager_ns:
            continue  # a NetworkPolicy only governs its own namespace
        if not selector_selects(spec.get("podSelector"), template_labels):
            continue
        allowed = False
        for rule in spec.get("ingress") or []:
            if not rule.get("ports"):
                if not rule.get("from"):
                    allowed = True  # fully unrestricted rule
                    break
                continue  # peer-restricted, port-unrestricted: not for the apiserver
            for port in rule.get("ports") or []:
                if rule.get("from"):
                    continue  # peer-restricted: cannot carry apiserver traffic
                if str(port.get("port")) in {str(p) for p in webhook_ports} and port.get("protocol", "TCP") == "TCP":
                    allowed = True
        if not allowed and webhook_ports:
            problems.append(
                f"{where(doc2)} selects the controller pod but allows no ingress on the "
                f"webhook port(s) {sorted(webhook_ports)}"
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
    either be rendered alongside it — in the workload's own namespace — or be
    an explicitly allowed external dependency (platform- or sibling-component
    provided). For rendered ConfigMaps the requested key is verified too."""
    allow = set(args.allow or [])
    docs = []
    for path in args.rendered:
        docs.extend(load_docs(path))

    rendered = {"ConfigMap": set(), "Secret": set()}  # (namespace, name)
    configmap_data_keys = {}  # (namespace, name) -> set(keys) in data only
    configmap_all_keys = {}  # (namespace, name) -> set(keys) in data + binaryData
    for doc in docs:
        kind = doc.get("kind")
        if kind in rendered:
            meta = doc.get("metadata") or {}
            key = (meta.get("namespace") or "default", meta.get("name"))
            rendered[kind].add(key)
            if kind == "ConfigMap":
                configmap_data_keys[key] = set((doc.get("data") or {}).keys())
                configmap_all_keys[key] = configmap_data_keys[key] | set(
                    (doc.get("binaryData") or {}).keys()
                )

    def resolves(kind, namespace, name):
        # Kubernetes resolves the reference in the pod's namespace; a same-named
        # object in another namespace does not satisfy it.
        return (namespace, name) in rendered[kind] or name in allow

    problems = []

    def check_ref(kind, namespace, ref, what, optional=None):
        if ref is None:
            return
        name = ref.get("name")
        if optional is None:
            optional = ref.get("optional")
        if optional:
            return
        if not resolves(kind, namespace, name):
            problems.append(f"{what} references {kind} {name} which is not rendered")
            return
        if kind == "ConfigMap" and (namespace, name) in configmap_all_keys:
            # Rendered alongside: the requested key must exist too (a misspelled
            # key blocks the container just as effectively). Env valueFrom only
            # reads `data`; volume `items` may also come from `binaryData`.
            key = ref.get("key")
            if key and key not in configmap_data_keys.get((namespace, name), set()):
                problems.append(
                    f"{what} references key {key} which {kind} {name} does not define"
                )
            for item in ref.get("items") or []:
                item_key = item.get("key")
                if item_key and item_key not in configmap_all_keys[(namespace, name)]:
                    problems.append(
                        f"{what} requests item key {item_key} which {kind} {name} does not define"
                    )

    for doc, pod_spec in pod_specs(docs):
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or "default"
        for container in (pod_spec.get("containers") or []) + (
            pod_spec.get("initContainers") or []
        ):
            what = f"{where(doc)} container {container.get('name')}"
            for entry in container.get("env") or []:
                source = entry.get("valueFrom") or {}
                env_what = f"{what}: env {entry.get('name')}"
                check_ref("ConfigMap", namespace, source.get("configMapKeyRef"), env_what)
                check_ref("Secret", namespace, source.get("secretKeyRef"), env_what)
            for source in container.get("envFrom") or []:
                env_what = f"{what}: envFrom"
                check_ref("ConfigMap", namespace, source.get("configMapRef"), env_what)
                check_ref("Secret", namespace, source.get("secretRef"), env_what)
        for volume in pod_spec.get("volumes") or []:
            what = f"{where(doc)}: volume {volume.get('name')}"
            check_ref("ConfigMap", namespace, volume.get("configMap"), what)
            secret = volume.get("secret")
            # Secret volumes identify the object with secretName, not name.
            if secret is not None:
                secret = {"name": secret.get("secretName"), "optional": secret.get("optional")}
            check_ref("Secret", namespace, secret, what)
            for source in ((volume.get("projected") or {}).get("sources") or []):
                check_ref(
                    "ConfigMap", namespace, source.get("configMap"),
                    f"{what} (projected)", source.get("configMap", {}).get("optional"),
                )
                check_ref(
                    "Secret", namespace, source.get("secret"),
                    f"{what} (projected)", source.get("secret", {}).get("optional"),
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
