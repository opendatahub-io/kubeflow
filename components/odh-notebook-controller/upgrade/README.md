## Motivation

Thinking about not restarting user workbenches is hard and this test should provide an automated check for that.

## Fidelity considerations

Beside this, we need to also run [odh-e2e test](https://github.com/skodjob/odh-e2e) that use actual controllers built as containers installed from OLM bundle on an OpenShift cluster.
That test is much more faithful to a real production deployment, although it's true we only run it with ephemeral short-lived single-node clusters.
The advantage of the test here is that it can be run very quickly from the project checkout directory with the current sourcecode.
It is enough to use `make envtest` and `go test` to run this, no building of images and no additional setup is required.
Meaning that it's possible to iterate on this quickly and still have a fairly comprehensive and realistic upgrade test.

# Test data for testing (simulated) operator upgrade

Each directory holds CRs dumped from a particular Red Hat OpenShift AI deployment.
We are using Red Hat OpenShift AI release versioning here to name the directories.

| ODH version | RHOAI version | opendatahub-io/kubeflow release |
|-------------|---------------|---------------------------------|
|             | 2.13          |                                 |
|             |               |                                 |

The test deploys (current) CRDs, then deploys (old) CRs that simulate a lived-in product installation, then starts (current) controllers and asserts that nothing adverse happened.

1. old CRs cannot be deployed with current CRDs
   * that means we made an incompatible changes to CRDs and that change must be reverted
2. current operator does not run correctly with old CRs
   * incompatible change in the operator must be reverted and done again correctly
3. current operator modifies old CRs in an impermissible way, such as if it modifies statefulset spec leading to notebook pod restart
   * incompatible change in the operator must be reverted and done again correctly

This way we will test upgrade from any old version to the current (in-development) version.
This should mean that no supported upgrade path will surprise us, because we have tested a superset.
If an upgrade from an older version is both unsupported and also actually not working correctly, only then we remove it from these tests.

We will test even upgrades that are not supported according to the various support policies of vendors.
If the upgrade can actually be performed correctly, that's fine; if not, we will remove the data directory, delete the test,
and leave a note in the README why upgroade is not working.

## More on envtest environment

These tests run in kubernetes testenv, which is a minimal Kubernetes distribution of just the etcd and kube-apiserver.
Notably it is without a kubelet and without other controllers, not even the [kube-controller-manager](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-controller-manager) is present.
OpenShift extensions are not present either, so we need to apply our own Route CRD.

Since kubelet is missing, no actual containers are being started when things get deployed to the kube-apiserver.
Furthermore, since kube-controller-manager is missing, there aren't even the standard controllers that create pods from deployments or statefulsets present.
That means that any deployed statefulset will lay untouched and pod resources are not created from it.

## Obtaining test data

Use the commands given below to dump an existing namespace on a working, deployed cluster for the purpose of upgrade testing.
Work in the Dashboard UI, we want to simulate a setup that an user might conceivable create.

1. in DSCi, set certificates to Managed
2. create DSC as the default one
3. in dashboard, work as the `developer` user and create a `developer` namespace
4. create a Data Connection, use the [AWS example credentials](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetAccessKeyInfo.html)
   * Id: `AKIAIOSFODNN7EXAMPLE`
   * Key: `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`
5. still as `developer`, spawn a Workbench

## Dumping command

Here's the commands that will dump what's in your `developer` namespace into yaml files for use in these tests.
When adding a new RHOAI version, place the dump into `data/rhoai-version` and commit the files.

```shell
# dumps all resources in `developer` namespace to YAMLs
kubectl api-resources --verbs=list --namespaced -o name > api-resources.txt
cat api-resources.txt | \
  grep -v '^events$' | grep -v '^events.events.k8s.io$' | grep -v '^packagemanifests.packages.operators.coreos.com$' | \
  xargs -n 1 kubectl get -o=name -n developer > resource-names.txt
cat resource-names.txt | while read line; do
  mkdir -p "$(dirname ${line})"
  kubectl get -o=yaml -n developer "${line}" > "${line}.yaml"
done
```

## Audit logging

In `data/configs` there is audit-policy for enabling audit logging from kube-apiserver.

One useful `jq` command to filter out events by type from the audit log is

```jq
select(
    .verb != "get" and .verb != "watch" and .verb != "list" and .verb != "create"
)
```
