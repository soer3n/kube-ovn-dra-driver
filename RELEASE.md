# Release Process

The kube-ovn NIC DRA driver is released on an as-needed basis. Published
release artifacts are:

- The `kube-ovn-dra-driver` Helm chart
- Container images

Both are published to GitHub Container Registry under `ghcr.io/soer3n`. The Helm
chart may be released independently from the container images, but when new
images are cut a matching chart release should usually be cut at the same time.

The process is:

1. Decide whether the container image, the Helm chart, or both should be
   released.
2. When releasing new container images, bump the Helm chart's `appVersion` in
   `deployments/helm/kube-ovn-dra-driver/Chart.yaml` to the image version being
   cut.
3. Tag and push:
    - Container images: a `v`-prefixed [SemVer] tag, e.g. `v0.1.0`
      (`make IMAGE_GIT_TAG` derives image tags from `v*` tags).
    - Helm chart: a `chart/`-prefixed [SemVer] tag, e.g. `chart/0.1.0`
      (the chart version is derived from `chart/*` tags).
    - The same commit may carry one tag of each form to release both at once.

      ```bash
      git tag -a v0.1.0 -m v0.1.0
      git tag -a chart/0.1.0 -m chart/0.1.0
      git push origin v0.1.0 chart/0.1.0
      ```
4. Build and push the artifacts (override the registry as needed):

    ```bash
    make REGISTRY=ghcr.io/soer3n push-release-artifacts
    ```
5. Draft and publish a [GitHub release][releases] with generated release notes.

[SemVer]: https://semver.org/
[releases]: https://github.com/soer3n/kube-ovn-dra-driver/releases
