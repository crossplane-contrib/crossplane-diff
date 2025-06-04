# crossplane-diff

# Local development

In order to run tests locally, you'll need to install `setup-envtest`.  You can run the following command to set up the environment:

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20
setup-envtest use 1.30.3 # or whatever cluster version we're using now
```

The setup command will download the necessary binaries and set up the environment for you.  It'll set 
`KUBEBUILDER_ASSETS` on the environment, so restart your terminal (or IDE) as necessary.

Next, run `go generate` with earthly to pull in the cluster manifests needed for the ITs/e2es:

```bash
earthly +go-generate --CROSSPLANE_IMAGE_TAG=(target crossplane version here)
```

Then you can run the tests with:

```bash
cd cmd/crank
go test ./...
```

