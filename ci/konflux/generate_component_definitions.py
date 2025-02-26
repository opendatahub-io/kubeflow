#!/usr/bin/env python3

import yaml


"""
This script is used to configure a Konflux Application with component definitions.
We have very many components, and clicking them one by one in the UI is too inefficient.

$ poetry run ci/cached-builds/konflux_generate_component_definitions.py > konflux_components.yaml
$ oc apply -f konflux_components.yaml

Open https://console.redhat.com/application-pipeline/workspaces/rhoai-ide-konflux/applications
and see the result in the "Components" tab.
"""

workspace_name = "rhoai-ide-konflux-tenant"
application_name = "kubeflow"
application_uid = "89959242-a304-41ef-9654-c360c415fbb9"
git_revision = "main"
git_url = "https://github.com/opendatahub-io/kubeflow"
pr_number = "514"


def konflux_component(component_name: str, context_path: str, dockerfile_path: str) -> dict:
    return {
        "apiVersion": "appstudio.redhat.com/v1alpha1",
        "kind": "Component",
        "metadata": {
            "annotations": {
                # this annotation will create imagerepository in quay,
                # https://redhat-internal.slack.com/archives/C07S8637ELR/p1736436093726049?thread_ts=1736420157.217379&cid=C07S8637ELR
                "image.redhat.com/generate": '{"visibility": "public"}',

                "build.appstudio.openshift.io/status": '{"pac":{"state":"enabled","merge-url":"' + git_url + '/pull/' + pr_number + '","configuration-time":"Tue, 18 Feb 2025 12:39:27 UTC"},"message":"done"}',
                "build.appstudio.openshift.io/pipeline": '{"name":"docker-build-oci-ta","bundle":"latest"}',
                "git-provider": "github",
                "git-provider-url": "https://github.com",
            },
            "name": component_name,
            "namespace": workspace_name,
            "ownerReferences": [
                {
                    "apiVersion": "appstudio.redhat.com/v1alpha1",
                    "kind": "Application",
                    "name": application_name,
                    "uid": application_uid,
                }
            ],
            "finalizers": [
                "test.appstudio.openshift.io/component",
                "pac.component.appstudio.openshift.io/finalizer",
            ],
        },
        "spec": {
            "application": application_name,
            "componentName": component_name,
            "containerImage": "quay.io/redhat-user-workloads/"
                              + workspace_name
                              + "/"
                              + component_name,
            "resources": {},
            "source": {
                "git": {
                    "context": context_path,
                    "dockerfileUrl": dockerfile_path,
                    "revision": git_revision,
                    "url": git_url,
                }
            },
        },
    }


def main():
        components = [
            konflux_component("kf-notebook-controller", context_path="components/", dockerfile_path="components/notebook-controller/Dockerfile"),
            konflux_component("odh-notebook-controller", context_path="components/", dockerfile_path="components/odh-notebook-controller/Dockerfile"),
        ]
        for component in components:
            print(yaml.dump(component, explicit_start=True))


if __name__ == "__main__":
    main()
