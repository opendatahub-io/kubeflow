# Installing The Latest OSSM Release

> **_NOTE:_** Scripts are derived from https://gitlab.cee.redhat.com/istio/kiali-qe/kiali-dev-tools/-/tree/master/install-scripts.

To install the latest release of OSSM (including Kiali), use the script `install-ossm-release.sh`. This will install the latest released images from the public Red Hat repository. In other words, this will install exactly what customers are installing.

Here's what you need to do in order to install OSSM.

First, install a default OpenShift cluster and make sure you do not already have Istio, Service Mesh, or Kiali installed.

Second, log into this cluster as a cluster admin user via 'oc'.

Now install the OSSM operators:

```
./install-ossm-release.sh --enable-kiali true install-operators
```

Once the operators have been given time to start up, now install a control plane with Istio and Kiali:

```
./install-ossm-release.sh --enable-kiali true install-smcp
```
