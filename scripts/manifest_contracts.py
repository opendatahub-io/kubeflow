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

# Every controller-builder watch call site must match one of the known
# argument forms: a &pkg.Type{} literal, optionally wrapped in
# client.Object(&pkg.Type{...}), or a bare variable assigned elsewhere in the
# file (possibly inside client.Object(...)). Any other form fails loudly: a
# silently skipped watch is an unchecked RBAC grant.
WATCH_CALL_START_RE = re.compile(r"\.\s*(For|Owns|Watches)\s*\(")
WATCH_LITERAL_ARG_RE = re.compile(
    r"^(?:client\.Object\s*\(\s*)?&([A-Za-z0-9_]+\.[A-Za-z0-9_]+)\s*\{"
)
WATCH_VARIABLE_ARG_RE = re.compile(
    r"^(?:client\.Object\s*\(\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*[,)]"
)
# Any assignment to a name, whatever the right-hand side: used to detect
# assignments this parser cannot interpret and fail instead of guessing.
ASSIGN_ANY_RE_TMPL = r"\b{name}\s*:?=\s*([^;\n]+)"
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


def mask_go_strings(source: str) -> str:
    """Blank the contents of string/rune/raw-string literals (apply after
    strip_go_source) so scans cannot match code that only exists inside a
    literal: `x := "obj := &corev1.Pod{}"` must not count as an assignment."""
    out = []
    i, n = 0, len(source)
    while i < n:
        c = source[i]
        if c in ('"', "'", "`"):
            quote = c
            j = i + 1
            while j < n:
                if quote != "`" and source[j] == "\\":
                    j += 2
                    continue
                if source[j] == quote:
                    break
                j += 1
            out.append(quote + quote)
            i = j + 1
        else:
            out.append(c)
            i += 1
    return "".join(out)


def watch_variable_types(source: str, masked: str, name: str, filename: str):
    """Map a variable name to the (apiGroup, plural) it watches.

    `source` is comment-stripped with string literals intact (needed to read
    the SetAPIVersion/SetKind arguments of the unstructured idiom); `masked`
    additionally has literal contents blanked, so assignment scans only see
    live code. Every assignment to the name must be one this parser
    understands — an unparseable one fails loudly instead of being skipped —
    and more than one distinct resolved type is an unresolvable ambiguity."""
    resolved = set()
    for assign in re.finditer(ASSIGN_ANY_RE_TMPL.format(name=re.escape(name)), masked):
        rhs = assign.group(1).strip()
        if rhs.startswith("="):
            continue  # `name == expr` is a comparison, not an assignment
        if rhs.startswith("&unstructured.Unstructured"):
            continue  # resolved through the Set* idiom below
        if rhs.startswith("&"):
            go_type = rhs[1:].split("{")[0].strip()
            gvk = WATCHED_KINDS.get(go_type)
            if gvk is None:
                fail(
                    f"{filename}: variable '{name}' is assigned '{go_type}', which is not "
                    f"a known watched kind: add it to WATCHED_KINDS in "
                    f"scripts/manifest_contracts.py"
                )
            resolved.add(gvk)
            continue
        fail(
            f"{filename}: variable '{name}' is assigned '{rhs[:50]}' which this parser "
            f"cannot resolve. Pass a &pkg.Type{{}} literal to the watch call, or teach "
            f"watch_variable_types in scripts/manifest_contracts.py to resolve it."
        )
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


def strip_go_source(source: str) -> str:
    """Remove comments so pattern scans see only live code: a commented-out
    assignment (`// obj := &corev1.Pod{}`) must not count as a candidate type.
    String literals are kept verbatim (a `//` inside one, e.g. a URL, is not
    a comment) because the unstructured watch idiom is resolved through the
    SetAPIVersion/SetKind call arguments."""
    out = []
    i, n = 0, len(source)
    while i < n:
        c = source[i]
        nxt = source[i + 1] if i + 1 < n else ""
        if c in ('"', "'", "`"):
            quote = c
            j = i + 1
            while j < n:
                if quote != "`" and source[j] == "\\":
                    j += 2
                    continue
                if source[j] == quote:
                    break
                j += 1
            out.append(source[i:min(j + 1, n)])
            i = j + 1
        elif c == "/" and nxt == "/":
            j = source.find("\n", i)
            i = n if j < 0 else j  # keep the newline itself
        elif c == "/" and nxt == "*":
            j = source.find("*/", i + 2)
            # Replace with a space: `var/* note */ns` must not become `varns`.
            out.append(" ")
            i = n if j < 0 else j + 2
        else:
            out.append(c)
            i += 1
    return "".join(out)


def parse_watches(component_dir: str):
    """Return {(apiGroup, plural): set(call types)} for every kind the
    controller-builder watches in the component's Go sources. Call sites with
    an argument this parser does not recognize fail the check: a silently
    skipped watch is an unchecked RBAC grant."""
    watches = {}
    controllers_dir = os.path.join(component_dir, "controllers")
    if not os.path.isdir(controllers_dir):
        fail(f"{component_dir}: no controllers/ directory to parse")
    if os.path.islink(controllers_dir):
        fail(f"{controllers_dir}: refusing a symlinked controllers directory")
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
            with open(path, encoding="utf-8") as handle:
                source = strip_go_source(handle.read())
            masked = mask_go_strings(source)
            for match in WATCH_CALL_START_RE.finditer(masked):
                call = match.group(1)
                rest = masked[match.end():match.end() + 200].lstrip()
                literal = WATCH_LITERAL_ARG_RE.match(rest)
                if literal:
                    gvk = WATCHED_KINDS.get(literal.group(1))
                    if gvk is None:
                        fail(
                            f"{name}: unknown watched kind {literal.group(1)} in {call}(): "
                            f"add it to WATCHED_KINDS in scripts/manifest_contracts.py"
                        )
                    watches.setdefault(gvk, set()).add(call)
                    continue
                variable = WATCH_VARIABLE_ARG_RE.match(rest)
                if variable:
                    var_name = variable.group(1)
                    gvk = watch_variable_types(source, masked, var_name, name)
                    if gvk is None:
                        fail(
                            f"{name}: {call}() is passed the variable '{var_name}' which has "
                            f"no assignment this parser can resolve. Pass a &pkg.Type{{}} "
                            f"literal, or teach watch_variable_types to resolve it."
                        )
                    watches.setdefault(gvk, set()).add(call)
                    continue
                fail(
                    f"{name}: {call}() is passed an argument of an unsupported form "
                    f"('{rest[:50].strip()}'). Pass a &pkg.Type{{}} literal (directly or "
                    f"through client.Object), or a variable assigned one, so the watch "
                    f"cannot be silently skipped."
                )
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
    """{(namespace, name)} for ServiceAccounts of controller manager workloads.

    A manager workload is one running a container named `manager` (the
    kubebuilder convention) — keyed on the container name, not the workload
    name: the kf Deployment is notebook-controller-deployment. Grants held by
    *other* rendered workloads (e.g. a culler or proxy) must not satisfy the
    manager's watch coverage."""
    accounts = set()
    for doc, pod_spec in pod_specs(docs):
        if not any(c.get("name") == "manager" for c in pod_spec.get("containers") or []):
            continue
        meta = doc.get("metadata") or {}
        namespace = meta.get("namespace") or "default"
        sa = pod_spec.get("serviceAccountName") or pod_spec.get("serviceAccount") or "default"
        accounts.add((namespace, sa))
    return accounts


def bound_role_rules(docs, accounts=None, cluster_wide_only=False):
    """Union of (apiGroup, resource) -> verbs over exactly the roles that
    reach the given ServiceAccounts (default: every rendered workload's): a
    role counts only when a binding whose subject is one of those accounts
    references it. An unbound (or differently bound) ClusterRole in the
    render grants the controller nothing.

    A ClusterRole referenced through a *RoleBinding* is deliberately not
    counted: that grants access only within the binding's namespace, which
    cannot back a cluster-wide list/watch informer. Rules restricted by
    `resourceNames` are excluded for the same reason.

    With cluster_wide_only=True, rules from namespaced Roles are excluded
    entirely: controller-runtime informers list/watch across the whole
    cluster, so a Role bound in one namespace authorizes them nowhere."""
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
            if cluster_wide_only:
                continue  # a namespaced Role cannot back a cluster-wide informer
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
    # authorize the manager's informers. And only cluster-wide grants count:
    # controller-runtime informers list/watch across the whole cluster, so a
    # namespaced Role authorizes them nowhere.
    rules = bound_role_rules(
        docs, accounts=manager_service_accounts(docs), cluster_wide_only=True
    )
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
    # targetPort -> backend container port), tracked per backend workload so
    # later checks evaluate the workload the Service actually selects.
    required_by_backend = {}  # workload index -> set(container ports)
    referenced_services = {}  # (ns, name) -> (svc doc, [backend indices])

    # The webhook server is the container named `manager` (the kubebuilder
    # convention, matching manager_containers()); ports and cert mounts on
    # other containers of the selected workload prove nothing.
    def manager_container(idx):
        for container in workloads[idx][3].get("containers") or []:
            if container.get("name") == "manager":
                return container
        return None

    DEFAULT_WEBHOOK_CERT_DIR = "/tmp/k8s-webhook-server/serving-certs"

    def manager_cert_dir(container):
        args = container.get("args") or []
        for arg in args:
            if arg.startswith("--webhook-cert-dir="):
                return arg.split("=", 1)[1]
        for i, arg in enumerate(args):
            if arg == "--webhook-cert-dir" and i + 1 < len(args):
                return args[i + 1]
        return DEFAULT_WEBHOOK_CERT_DIR

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
            selector = (svc.get("spec") or {}).get("selector") or {}
            if not selector:
                problems.append(
                    f"Service/{key[1]} has no selector; nothing routes the webhook traffic"
                )
                continue
            backend_indices = [
                idx for idx, w in enumerate(workloads)
                if w[1] == key[0] and all(w[2].get(k) == v for k, v in selector.items())
            ]
            if not backend_indices:
                problems.append(
                    f"Service/{key[1]} selector {selector} matches no rendered workload"
                )
                continue
            referenced_services[key] = (svc, backend_indices)
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
            listens = []
            for idx in backend_indices:
                container = manager_container(idx)
                if container is None:
                    problems.append(
                        f"Service/{key[1]} selects {where(workloads[idx][0])} which has no "
                        f"container named manager to serve the webhook"
                    )
                    continue
                for port in container.get("ports") or []:
                    if (
                        port.get("name") == str(target) or port.get("containerPort") == target
                    ) and isinstance(port.get("containerPort"), int) \
                            and port.get("protocol", "TCP") == "TCP":
                        listens.append((idx, port["containerPort"]))
            if not listens:
                problems.append(
                    f"Service/{key[1]} targetPort {target} matches no manager container port "
                    f"on the workload its selector selects"
                )
            else:
                for idx, port in listens:
                    required_by_backend.setdefault(idx, set()).add(port)

    # 2. Every Service referenced by a webhook must request a serving cert, and
    #    the manager container of each backend it selects must mount that exact
    #    secret (RHOAIENG-84738) at the directory the manager is configured to
    #    read it from (--webhook-cert-dir, default
    #    /tmp/k8s-webhook-server/serving-certs): a secret mounted anywhere else
    #    cannot be read by the webhook server. Follow the container's
    #    volumeMounts, not the pod's volumes: a volume without a mount serves
    #    nobody. Each referenced Service is validated on its own against its
    #    own backends: one correctly annotated Service must not cover for
    #    another that lost its annotation.
    def backend_secret_mounts(idx):
        """{secretName: mountPath} for the manager container of a backend."""
        container = manager_container(idx)
        if container is None:
            return {}
        _, _, _, pod_spec = workloads[idx]
        volumes_by_name = {v.get("name"): v for v in pod_spec.get("volumes") or []}
        mounted = {}
        for mount in container.get("volumeMounts") or []:
            volume = volumes_by_name.get(mount.get("name")) or {}
            secret = (volume.get("secret") or {}).get("secretName")
            if secret:
                mounted[secret] = mount.get("mountPath")
        return mounted

    for (svc_ns, svc_name), (svc, backend_indices) in sorted(referenced_services.items()):
        annotations = (svc.get("metadata") or {}).get("annotations") or {}
        secret_name = annotations.get("service.beta.openshift.io/serving-cert-secret-name")
        if not secret_name:
            problems.append(
                f"Service/{svc_name} is referenced by a webhook but carries no "
                f"serving-cert annotation"
            )
            continue
        for idx in backend_indices:
            container = manager_container(idx)
            if container is None:
                continue  # already reported in section 1
            mounted = backend_secret_mounts(idx)
            cert_dir = manager_cert_dir(container)
            if not mounted:
                problems.append(
                    f"backend {where(workloads[idx][0])} (selected by Service/{svc_name}) "
                    f"mounts no secret volumes at all"
                )
            elif secret_name not in mounted:
                problems.append(
                    f"serving-cert secret {secret_name} requested by Service/{svc_name} is not "
                    f"mounted by {where(workloads[idx][0])} (mounted: {sorted(mounted)})"
                )
            elif mounted[secret_name] != cert_dir:
                problems.append(
                    f"serving-cert secret {secret_name} is mounted at "
                    f"{mounted[secret_name]} but the manager reads --webhook-cert-dir "
                    f"{cert_dir}: the webhook server cannot read the certificate"
                )

    # 3. A NetworkPolicy that selects a webhook backend must allow the webhook
    #    port (RHOAIENG-99636: default-deny NP blocked the webhook).
    #    * The policy only applies in its own namespace; an empty podSelector
    #      selects every pod in it; matchExpressions are evaluated against the
    #      backend's labels.
    #    * The port that must be allowed is the one the webhook Service
    #      actually targets on that backend (section 1), not any port the
    #      manager happens to declare.
    #    * Coverage is aggregated over *all* selecting policies before being
    #      compared to the required ports: a default-deny policy alongside an
    #      allow rule is a valid configuration, and every required port must
    #      be covered by the combination.
    #    * A port rule with `from`/`to` peers cannot be relied on for
    #      kube-apiserver traffic (it originates outside any selectable pod
    #      network), so only unrestricted-source port rules count.
    if not required_by_backend:
        # No service-based webhook resolved its backend port (section 1
        # already reports that). Fall back to ports declared for the webhook
        # on manager workloads so the NetworkPolicy guard still applies.
        for idx, (_, _, _, pod_spec) in enumerate(workloads):
            if not any(c.get("name") == "manager" for c in pod_spec.get("containers") or []):
                continue
            for container in pod_spec.get("containers") or []:
                for port in container.get("ports") or []:
                    if port.get("name") == "webhook" and isinstance(port.get("containerPort"), int):
                        required_by_backend.setdefault(idx, set()).add(port["containerPort"])
        if not required_by_backend and not problems:
            problems.append("no manager workload declares a port named webhook")
    for idx, required in sorted(required_by_backend.items()):
        backend_doc, backend_ns, backend_labels, _ = workloads[idx]
        selecting_policies = []
        for doc2 in docs:
            if doc2.get("kind") != "NetworkPolicy":
                continue
            spec = doc2.get("spec") or {}
            if "Ingress" not in (spec.get("policyTypes") or ["Ingress"]):
                continue
            meta = doc2.get("metadata") or {}
            if (meta.get("namespace") or "default") != backend_ns:
                continue  # a NetworkPolicy only governs its own namespace
            if not selector_selects(spec.get("podSelector"), backend_labels):
                continue
            selecting_policies.append(doc2)
        if not selecting_policies:
            continue  # nothing isolates this backend; ingress is unrestricted
        # Named ports in rules resolve on this backend's own container ports.
        backend_named_ports = {
            port.get("name"): port["containerPort"]
            for container in workloads[idx][3].get("containers") or []
            for port in container.get("ports") or []
            if port.get("name") and isinstance(port.get("containerPort"), int)
            and port.get("protocol", "TCP") == "TCP"
        }
        unrestricted = False
        covered_all = False
        covered = set()  # exact TCP port numbers
        ranges = []  # (start, end) TCP port ranges from endPort
        for doc2 in selecting_policies:
            for rule in (doc2.get("spec") or {}).get("ingress") or []:
                if not rule.get("ports"):
                    if not rule.get("from"):
                        unrestricted = True  # fully unrestricted rule
                    continue  # peer-restricted, port-unrestricted
                if rule.get("from"):
                    continue  # peer-restricted: cannot carry apiserver traffic
                for entry in rule.get("ports") or []:
                    if entry.get("protocol", "TCP") != "TCP":
                        continue
                    port = entry.get("port")
                    if port is None:
                        covered_all = True  # a TCP rule without a port covers every TCP port
                        continue
                    if isinstance(port, str):
                        number = backend_named_ports.get(port)
                        if number is not None:
                            covered.add(number)
                        continue
                    end = entry.get("endPort")
                    if isinstance(end, int) and end >= port:
                        ranges.append((port, end))
                    else:
                        covered.add(port)
        if unrestricted or covered_all:
            continue
        missing = {
            p for p in required
            if p not in covered and not any(a <= p <= b for a, b in ranges)
        }
        if missing:
            problems.append(
                f"NetworkPolicies selecting {where(backend_doc)} allow no ingress on the "
                f"webhook port(s) {sorted(missing)}"
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
