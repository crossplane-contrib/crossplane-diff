# See https://docs.earthly.dev/docs/earthfile/features
VERSION --try --raw-output 0.8

PROJECT crossplane-contrib/crossplane-diff

ARG --global GO_VERSION=1.25.5

fetch-crossplane-clusters:
  BUILD +fetch-crossplane-cluster \
    --CROSSPLANE_IMAGE_TAG=release-1.20 \
    --CROSSPLANE_IMAGE_TAG=main

# fetch-crossplane-cluster fetches the cluster directory from crossplane/crossplane
# at the git revision corresponding to CROSSPLANE_IMAGE_TAG
fetch-crossplane-cluster:
  ARG CROSSPLANE_IMAGE_TAG=main
  ARG CROSSPLANE_REPO=https://github.com/crossplane/crossplane.git
  ARG SAVE_LOCALLY=true
  FROM alpine/git:latest
  WORKDIR /src

  # Clone the crossplane repository at the specified tag/revision
  RUN git clone --depth 1 --branch ${CROSSPLANE_IMAGE_TAG} ${CROSSPLANE_REPO} crossplane || \
      (git clone ${CROSSPLANE_REPO} crossplane && \
       cd crossplane && \
       git checkout ${CROSSPLANE_IMAGE_TAG})

  # Prefix the cluster directory with the tag/revision
  RUN mv crossplane/cluster crossplane/cluster-tmp
  RUN mkdir -p crossplane/cluster
  RUN mv crossplane/cluster-tmp crossplane/cluster/${CROSSPLANE_IMAGE_TAG}

  # Save the cluster directory as an artifact
  SAVE ARTIFACT crossplane/cluster/${CROSSPLANE_IMAGE_TAG}
  # Conditionally save locally (disabled for matrix builds)
  IF [ "$SAVE_LOCALLY" = "true" ]
    SAVE ARTIFACT crossplane/cluster/${CROSSPLANE_IMAGE_TAG} AS LOCAL cluster/${CROSSPLANE_IMAGE_TAG}
  END

# reviewable checks that a branch is ready for review. Run it before opening a
# pull request. It will catch a lot of the things our CI workflow will catch.
reviewable:
  WAIT
    BUILD +generate
  END
  BUILD +lint
  BUILD +test

# test runs unit tests.
test:
  BUILD +go-test

# lint runs linters.
lint:
  BUILD +go-lint

# build builds Crossplane for your native OS and architecture.
build:
  ARG USERPLATFORM
  BUILD --platform=$USERPLATFORM +go-build

# multiplatform-build builds Crossplane for all supported OS and architectures.
multiplatform-build:
  ARG RELEASE_ARTIFACTS=false
  BUILD +go-multiplatform-build --RELEASE_ARTIFACTS=${RELEASE_ARTIFACTS}

# generate runs code generation. To keep builds fast, it doesn't run as part of
# the build target. It's important to run it explicitly when code needs to be
# generated, for example when you update an API type.
generate:
  BUILD +go-modules-tidy

e2e-matrix:
  BUILD +e2e-check-host
  BUILD +e2e \
    --CROSSPLANE_IMAGE_TAG=release-1.20 \
    --CROSSPLANE_IMAGE_TAG=main \
    --SAVE_LOCALLY=false

# ci-e2e-matrix is used by CI to run e2e tests without the host cluster check.
# CI environments always start with a clean state, so the check is unnecessary.
ci-e2e-matrix:
  BUILD +ci-e2e \
    --CROSSPLANE_IMAGE_TAG=release-1.20 \
    --CROSSPLANE_IMAGE_TAG=main \
    --SAVE_LOCALLY=false

# e2e-check-host verifies that the host doesn't have kind clusters that would interfere with DIND.
# The nested containerization (host Docker -> earthly DIND -> kind) has limited cgroup capacity.
# Existing host kind clusters consume significant resources, preventing DIND from creating its own
# kind cluster. This particularly affects resource-constrained environments like Rancher Desktop.
e2e-check-host:
  LOCALLY
  RUN if command -v kind >/dev/null 2>&1 && [ -n "$(kind get clusters 2>/dev/null)" ]; then \
    echo "ERROR: Found existing kind clusters on host."; \
    echo "These consume cgroup resources needed by earthly DIND to create its own kind cluster."; \
    echo ""; \
    echo "Please delete them first:"; \
    echo "  kind get clusters | xargs -r kind delete cluster --name"; \
    echo ""; \
    echo "Note: CI matrix builds work fine because each gets a clean environment."; \
    exit 1; \
  fi

# e2e runs end-to-end tests. See test/e2e/README.md for details.
# For local use - includes host cluster check.
e2e:
  ARG TEST_FORMAT=standard-verbose
  BUILD +e2e-check-host
  BUILD +e2e-internal

# ci-e2e runs end-to-end tests without host cluster check.
# For CI use only - skips the LOCALLY check to work in strict mode.
ci-e2e:
  BUILD +e2e-internal

# e2e-internal contains the actual e2e test implementation.
# Called by both e2e and ci-e2e targets.
e2e-internal:
  ARG TARGETARCH
  ARG TARGETOS
  ARG CROSSPLANE_IMAGE_TAG=main
  ARG SAVE_LOCALLY=true
  ARG GOARCH=${TARGETARCH}
  ARG GOOS=${TARGETOS}
  ARG FLAGS="-test-suite=base -labels=crossplane-version=${CROSSPLANE_IMAGE_TAG}"
  ARG TEST_FORMAT=testname
  ARG E2E_DUMP_EXPECTED
  # Using earthly image to allow compatibility with different development environments e.g. WSL
  FROM earthly/dind:alpine-3.20-docker-26.1.5-r0
  RUN wget https://dl.google.com/go/go${GO_VERSION}.${GOOS}-${GOARCH}.tar.gz
  RUN tar -C /usr/local -xzf go${GO_VERSION}.${GOOS}-${GOARCH}.tar.gz
  ENV GOTOOLCHAIN=local
  ENV GOPATH /go
  ENV PATH $GOPATH/bin:/usr/local/go/bin:$PATH
  RUN apk add --no-cache jq
  COPY +helm-setup/helm /usr/local/bin/helm
  COPY +kind-setup/kind /usr/local/bin/kind
  COPY +gotestsum-setup/gotestsum /usr/local/bin/gotestsum
  # go-build is needed to create the `crossplane-diff` binary that we use in some of the e2es.
  # technically this will break if we ever used a windows builder (since this would be crossplane-diff.exe)
  # but we don't so it's fine for now.
  COPY +go-build/crossplane-diff .
  COPY +go-build-e2e/e2e .
  # Fetch the cluster directory from the crossplane repo at the specified tag
  COPY (+fetch-crossplane-cluster/${CROSSPLANE_IMAGE_TAG} --CROSSPLANE_IMAGE_TAG=${CROSSPLANE_IMAGE_TAG} --SAVE_LOCALLY=${SAVE_LOCALLY}) cluster/${CROSSPLANE_IMAGE_TAG}
  BUILD +patch-crds --CROSSPLANE_IMAGE_TAG=${CROSSPLANE_IMAGE_TAG}
  COPY --dir test .
  TRY
    # Note: The crossplane-diff binary version (CROSSPLANE_DIFF_VERSION) is not
    # passed here to allow Earthly to cache E2E runs as long as code doesn't change.
    # If the version contains a git commit, the cache would be invalidated on every commit.
    WITH DOCKER --pull crossplane/crossplane:${CROSSPLANE_IMAGE_TAG}
      # TODO(negz:) Set GITHUB_ACTIONS=true and use RUN --raw-output when
      # https://github.com/earthly/earthly/issues/4143 is fixed.
      RUN E2E_DUMP_EXPECTED=${E2E_DUMP_EXPECTED} gotestsum --no-color=false --format ${TEST_FORMAT} --junitfile e2e-tests.xml --raw-command go tool test2json -t -p E2E ./e2e -test.v -crossplane-image=crossplane/crossplane:${CROSSPLANE_IMAGE_TAG} ${FLAGS}
    END
  FINALLY
    SAVE ARTIFACT --if-exists e2e-tests.xml AS LOCAL _output/tests/e2e-tests.xml
    SAVE ARTIFACT --if-exists test/e2e/manifests/beta/diff AS LOCAL test/e2e/manifests/beta/diff
  END

# go-modules downloads Crossplane's go modules. It's the base target of most Go
# related target (go-build, etc).
go-modules:
  ARG NATIVEPLATFORM
  FROM --platform=${NATIVEPLATFORM} golang:${GO_VERSION}
  WORKDIR /crossplane-diff
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY go.mod go.sum ./
  RUN go mod download
  SAVE ARTIFACT go.mod AS LOCAL go.mod
  SAVE ARTIFACT go.sum AS LOCAL go.sum

# go-modules-tidy tidies and verifies go.mod and go.sum.
go-modules-tidy:
  FROM +go-modules
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY --dir cmd/ internal/ test/ .
  RUN go mod tidy
  RUN go mod verify
  SAVE ARTIFACT go.mod AS LOCAL go.mod
  SAVE ARTIFACT go.sum AS LOCAL go.sum

# patch-crds patches CRDs fetched from crossplane/crossplane.  used to be called go-generate, but we don't actually
# do any go generation here anymore.  we can't run this under the upper-level +generate target because it generates
# changes under the /cluster directory, which while gitignored will fail the PR check for changed generated files.
patch-crds:
  ARG CROSSPLANE_IMAGE_TAG=main
  FROM +go-modules
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY +kubectl-setup/kubectl /usr/local/bin/kubectl
  # Fetch the cluster directory from the crossplane repo at the specified tag
  COPY (+fetch-crossplane-cluster/${CROSSPLANE_IMAGE_TAG} --CROSSPLANE_IMAGE_TAG=${CROSSPLANE_IMAGE_TAG}) cluster/${CROSSPLANE_IMAGE_TAG}
  # TODO(negz): Can this move into generate.go? Ideally it would live there with
  # the code that actually generates the CRDs, but it depends on kubectl.
  RUN kubectl patch --local --type=json \
    --patch-file cluster/${CROSSPLANE_IMAGE_TAG}/crd-patches/pkg.crossplane.io_deploymentruntimeconfigs.yaml \
    --filename cluster/${CROSSPLANE_IMAGE_TAG}/crds/pkg.crossplane.io_deploymentruntimeconfigs.yaml \
    --output=yaml > /tmp/patched.yaml \
    && mv /tmp/patched.yaml cluster/${CROSSPLANE_IMAGE_TAG}/crds/pkg.crossplane.io_deploymentruntimeconfigs.yaml
  SAVE ARTIFACT cluster/${CROSSPLANE_IMAGE_TAG}/crds AS LOCAL cluster/${CROSSPLANE_IMAGE_TAG}/crds
  SAVE ARTIFACT cluster/${CROSSPLANE_IMAGE_TAG}/meta AS LOCAL cluster/${CROSSPLANE_IMAGE_TAG}/meta

# go-build builds Crossplane binaries for your native OS and architecture.
# Set RELEASE_ARTIFACTS=true to output flat release-ready artifacts to _output/release/
go-build:
  ARG EARTHLY_GIT_SHORT_HASH
  ARG EARTHLY_GIT_COMMIT_TIMESTAMP
  ARG CROSSPLANE_DIFF_VERSION=v0.0.0-${EARTHLY_GIT_COMMIT_TIMESTAMP}-${EARTHLY_GIT_SHORT_HASH}
  ARG TARGETARCH
  ARG TARGETOS
  ARG GOARCH=${TARGETARCH}
  ARG GOOS=${TARGETOS}
  ARG LDFLAGS="-s -w -X=github.com/crossplane-contrib/crossplane-diff/internal/version.version=${CROSSPLANE_DIFF_VERSION}"
  ARG CGO_ENABLED=0
  ARG BIN_NAME=crossplane-diff
  ARG RELEASE_ARTIFACTS=false
  FROM +go-modules
  LET ext = ""
  IF [ "$GOOS" = "windows" ]
    SET ext = ".exe"
  END
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY --dir cmd/ internal/ .
  RUN go build -ldflags="${LDFLAGS}" -o ${BIN_NAME}${ext} ./cmd/diff
  RUN sha256sum ${BIN_NAME}${ext} | head -c 64 > ${BIN_NAME}${ext}.sha256
  RUN tar -czvf ${BIN_NAME}.tar.gz ${BIN_NAME}${ext} ${BIN_NAME}${ext}.sha256
  RUN sha256sum ${BIN_NAME}.tar.gz | head -c 64 > ${BIN_NAME}.tar.gz.sha256
  IF [ "$RELEASE_ARTIFACTS" = "true" ]
    # Flat structure with arch suffix for releases: _output/release/crossplane-diff_linux_amd64
    SAVE ARTIFACT --keep-ts ${BIN_NAME}${ext} AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}${ext}
    SAVE ARTIFACT --keep-ts ${BIN_NAME}${ext}.sha256 AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}${ext}.sha256
    SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}.tar.gz
    SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz.sha256 AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}.tar.gz.sha256
  ELSE
    # Nested structure for local development: _output/bin/linux_amd64/crossplane-diff
    SAVE ARTIFACT --keep-ts ${BIN_NAME}${ext} AS LOCAL _output/bin/${GOOS}_${GOARCH}/${BIN_NAME}${ext}
    SAVE ARTIFACT --keep-ts ${BIN_NAME}${ext}.sha256 AS LOCAL _output/bin/${GOOS}_${GOARCH}/${BIN_NAME}${ext}.sha256
    SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz AS LOCAL _output/bundle/${GOOS}_${GOARCH}/${BIN_NAME}.tar.gz
    SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz.sha256 AS LOCAL _output/bundle/${GOOS}_${GOARCH}/${BIN_NAME}.tar.gz.sha256
  END

# go-multiplatform-build builds Crossplane binaries for all supported OS
# and architectures. Set RELEASE_ARTIFACTS=true for flat release structure.
go-multiplatform-build:
  ARG RELEASE_ARTIFACTS=false
  BUILD \
    --platform=linux/amd64 \
    --platform=linux/arm64 \
    --platform=linux/arm \
    --platform=linux/ppc64le \
    --platform=darwin/arm64 \
    --platform=darwin/amd64 \
    --platform=windows/amd64 \
    +go-build --RELEASE_ARTIFACTS=${RELEASE_ARTIFACTS}

# go-build-e2e builds Crossplane's end-to-end tests.
go-build-e2e:
  ARG CGO_ENABLED=0
  FROM +go-modules
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY --dir test/ .
  RUN go test -c -o e2e ./test/e2e
  SAVE ARTIFACT e2e

# go-test runs Go unit tests.
go-test:
  ARG KUBE_VERSION=1.30.3
  ARG CROSSPLANE_IMAGE_TAG=main
  BUILD +fetch-crossplane-cluster
  BUILD +patch-crds
  FROM +go-modules
  DO github.com/earthly/lib+INSTALL_DIND
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY --dir cmd/ internal/ .
  # Fetch the cluster directory from the crossplane repo at the specified tag
  COPY (+fetch-crossplane-cluster/${CROSSPLANE_IMAGE_TAG} --CROSSPLANE_IMAGE_TAG=${CROSSPLANE_IMAGE_TAG}) cluster/${CROSSPLANE_IMAGE_TAG}
  COPY --dir +envtest-setup/envtest /usr/local/kubebuilder/bin
  # a bit dirty but preload the cache with the images we use in IT (found in functions.yaml)
  WITH DOCKER \
    --pull xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.11.0 \
    --pull xpkg.crossplane.io/crossplane-contrib/function-auto-ready:v0.4.2 \
    --pull xpkg.crossplane.io/crossplane-contrib/function-environment-configs:v0.4.0 \
    --pull xpkg.crossplane.io/crossplane-contrib/function-extra-resources:v0.2.0
    # this is silly, but we put these files into the default KUBEBUILDER_ASSETS location, because if we set
    # KUBEBUILDER_ASSETS on `go test` to the artifact path, which is perhaps more intuitive, the syntax highlighting
    # in intellij breaks due to the word BUILD in all caps.
    RUN go test -covermode=count -coverprofile=coverage.txt ./...
  END
  SAVE ARTIFACT coverage.txt AS LOCAL _output/tests/coverage.txt

# go-lint lints Go code.
go-lint:
  ARG GOLANGCI_LINT_VERSION=v2.7.2
  FROM +go-modules
  # This cache is private because golangci-lint doesn't support concurrent runs.
  CACHE --id go-lint --sharing private /root/.cache/golangci-lint
  CACHE --id go-build --sharing shared /root/.cache/go-build
  RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin ${GOLANGCI_LINT_VERSION}
  COPY .golangci.yml .
  COPY --dir cmd/ internal/ test/ .
  RUN golangci-lint run --fix
  SAVE ARTIFACT cmd AS LOCAL cmd
  SAVE ARTIFACT internal AS LOCAL internal
  SAVE ARTIFACT test AS LOCAL test

# envtest-setup is used by other targets to setup envtest.
envtest-setup:
  ARG KUBE_VERSION=1.30.3
  ARG TARGETOS
  ARG TARGETARCH
  FROM +go-modules
  CACHE --id go-build --sharing shared /root/.cache/go-build
  # pin for golang 1.23.  when upgrading to 1.24, upgrade this version to latest(ish):
  RUN go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20
  RUN setup-envtest use ${KUBE_VERSION} --os ${TARGETOS} --arch ${TARGETARCH} --bin-dir ./envtest
  SAVE ARTIFACT ./envtest/k8s/${KUBE_VERSION}-${TARGETOS}-${TARGETARCH} ./envtest

# kubectl-setup is used by other targets to setup kubectl.
kubectl-setup:
  ARG KUBECTL_VERSION=v1.35.0
  ARG NATIVEPLATFORM
  ARG TARGETOS
  ARG TARGETARCH
  FROM --platform=${NATIVEPLATFORM} curlimages/curl:8.17.0
  RUN curl -fsSL https://dl.k8s.io/${KUBECTL_VERSION}/kubernetes-client-${TARGETOS}-${TARGETARCH}.tar.gz|tar zx
  SAVE ARTIFACT kubernetes/client/bin/kubectl

# kind-setup is used by other targets to setup kind.
kind-setup:
  ARG KIND_VERSION=v0.31.0
  ARG NATIVEPLATFORM
  ARG TARGETOS
  ARG TARGETARCH
  FROM --platform=${NATIVEPLATFORM} curlimages/curl:8.17.0
  RUN curl -fsSLo kind https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${TARGETOS}-${TARGETARCH}&&chmod +x kind
  SAVE ARTIFACT kind

# gotestsum-setup is used by other targets to setup gotestsum.
gotestsum-setup:
  ARG GOTESTSUM_VERSION=1.13.0
  ARG NATIVEPLATFORM
  ARG TARGETOS
  ARG TARGETARCH
  FROM --platform=${NATIVEPLATFORM} curlimages/curl:8.17.0
  RUN curl -fsSL https://github.com/gotestyourself/gotestsum/releases/download/v${GOTESTSUM_VERSION}/gotestsum_${GOTESTSUM_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz|tar zx>gotestsum
  SAVE ARTIFACT gotestsum

# helm-docs-setup is used by other targets to setup helm-docs.
helm-docs-setup:
  ARG HELM_DOCS_VERSION=1.14.2
  ARG NATIVEPLATFORM
  ARG TARGETOS
  ARG TARGETARCH
  FROM --platform=${NATIVEPLATFORM} curlimages/curl:8.17.0
  IF [ "${TARGETARCH}" = "amd64" ]
    LET ARCH=x86_64
  ELSE
    LET ARCH=${TARGETARCH}
  END
  RUN curl -fsSL https://github.com/norwoodj/helm-docs/releases/download/v${HELM_DOCS_VERSION}/helm-docs_${HELM_DOCS_VERSION}_${TARGETOS}_${ARCH}.tar.gz|tar zx>helm-docs
  SAVE ARTIFACT helm-docs

# helm-setup is used by other targets to setup helm.
helm-setup:
  ARG HELM_VERSION=v4.0.4
  ARG NATIVEPLATFORM
  ARG TARGETOS
  ARG TARGETARCH
  FROM --platform=${NATIVEPLATFORM} curlimages/curl:8.17.0
  RUN curl -fsSL https://get.helm.sh/helm-${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz|tar zx --strip-components=1
  SAVE ARTIFACT helm

# Targets below this point are intended only for use in GitHub Actions CI. They
# may not work outside of that environment. For example they may depend on
# secrets that are only availble in the CI environment. Targets below this point
# must be prefixed with ci-.

# TODO(negz): Is there a better way to determine the Crossplane version?
# This versioning approach maintains compatibility with the build submodule. See
# https://github.com/crossplane/build/blob/231258/makelib/common.mk#L205. This
# approach is problematic in Earthly because computing it inside a containerized
# target requires copying the entire git repository into the container. Doing so
# would invalidate all dependent target caches any time any file in git changed.

# ci-version is used by CI to set the CROSSPLANE_VERSION environment variable.
ci-version:
  LOCALLY
  RUN echo "CROSSPLANE_VERSION=$(git describe --dirty --always --tags|sed -e 's/-/./2g')" > $GITHUB_ENV

# ci-artifacts is used by CI to build and push the Crossplane image, chart, and
# binaries.
ci-artifacts:
  BUILD +multiplatform-build \
    --CROSSPLANE_REPO=index.docker.io/crossplane-contrib/crossplane-diff \
    --CROSSPLANE_REPO=ghcr.io/crossplane-contrib/crossplane-diff \
    --CROSSPLANE_REPO=xpkg.crossplane.io/crossplane-contrib/crossplane-diff

# ci-codeql-setup sets up CodeQL for the ci-codeql target.
ci-codeql-setup:
  ARG CODEQL_VERSION=2.23.8
  FROM curlimages/curl:8.17.0
  RUN curl -fsSL https://github.com/github/codeql-action/releases/download/codeql-bundle-v${CODEQL_VERSION}/codeql-bundle-linux64.tar.gz|tar zx
  SAVE ARTIFACT codeql

# ci-codeql is used by CI to build Crossplane with CodeQL scanning enabled.
ci-codeql:
  ARG CGO_ENABLED=0
  ARG TARGETOS
  ARG TARGETARCH
  # Note: Using a static version for caching. If the version contains a git commit,
  # the build layer cache would be invalidated on every commit.
  FROM +go-modules
  IF [ "${TARGETARCH}" = "arm64" ] && [ "${TARGETOS}" = "linux" ]
    RUN --no-cache echo "CodeQL doesn't support Linux on Apple Silicon" && false
  END
  COPY --dir +ci-codeql-setup/codeql /codeql
  CACHE --id go-build --sharing shared /root/.cache/go-build
  COPY --dir cmd/ internal/ .
  RUN /codeql/codeql database create /codeqldb --language=go
  RUN /codeql/codeql database analyze /codeqldb --threads=0 --format=sarif-latest --output=go.sarif --sarif-add-baseline-file-info
  SAVE ARTIFACT go.sarif AS LOCAL _output/codeql/go.sarif

# ci-promote-image is used by CI to promote a Crossplane image to a channel.
# In practice, this means creating a new channel tag (e.g. master or stable)
# that points to the supplied version.
ci-promote-image:
  ARG --required CROSSPLANE_REPO
  ARG --required CROSSPLANE_VERSION
  ARG --required CHANNEL
  FROM alpine:3.23
  RUN apk add docker
  # We need to omit the registry argument when we're logging into Docker Hub.
  # Otherwise login will appear to succeed, but buildx will fail on auth.
  IF [[ "${CROSSPLANE_REPO}" == *docker.io/* ]]
    RUN --secret DOCKER_USER --secret DOCKER_PASSWORD docker login -u ${DOCKER_USER} -p ${DOCKER_PASSWORD}
  ELSE
    RUN --secret DOCKER_USER --secret DOCKER_PASSWORD docker login -u ${DOCKER_USER} -p ${DOCKER_PASSWORD} ${CROSSPLANE_REPO}
  END
  RUN --push docker buildx imagetools create \
    --tag ${CROSSPLANE_REPO}:${CHANNEL} \
    --tag ${CROSSPLANE_REPO}:${CROSSPLANE_VERSION}-${CHANNEL} \
    ${CROSSPLANE_REPO}:${CROSSPLANE_VERSION}

# TODO(negz): Ideally ci-push-build-artifacts would be merged into ci-artifacts,
# i.e. just build and push them all in the same target. Currently we're relying
# on the fact that ci-artifacts does a bunch of SAVE ARTIFACT AS LOCAL, which
# ci-push-build-artifacts then loads. That's an anti-pattern in Earthly. We're
# supposed to use COPY instead, but I'm not sure how to COPY artifacts from a
# matrix build.

# ci-push-build-artifacts is used by CI to push binary artifacts to S3.
ci-push-build-artifacts:
  ARG --required CROSSPLANE_VERSION
  ARG --required BUILD_DIR
  ARG ARTIFACTS_DIR=_output
  ARG BUCKET_RELEASES=crossplane.releases
  ARG AWS_DEFAULT_REGION
  FROM amazon/aws-cli:2.32.28
  COPY --dir ${ARTIFACTS_DIR} artifacts
  RUN --push --secret=AWS_ACCESS_KEY_ID --secret=AWS_SECRET_ACCESS_KEY aws s3 sync --delete --only-show-errors artifacts s3://${BUCKET_RELEASES}/build/${BUILD_DIR}/${CROSSPLANE_VERSION}

# ci-promote-build-artifacts is used by CI to promote binary artifacts and Helm
# charts to a channel. In practice, this means copying them from one S3
# directory to another.
ci-promote-build-artifacts:
  ARG --required CROSSPLANE_VERSION
  ARG --required BUILD_DIR
  ARG --required CHANNEL
  ARG HELM_REPO_URL=https://charts.crossplane.io
  ARG BUCKET_RELEASES=crossplane.releases
  ARG BUCKET_CHARTS=crossplane.charts
  ARG PRERELEASE=false
  ARG AWS_DEFAULT_REGION
  FROM amazon/aws-cli:2.32.28
  RUN --secret=AWS_ACCESS_KEY_ID --secret=AWS_SECRET_ACCESS_KEY aws s3 sync --only-show-errors s3://${BUCKET_RELEASES}/build/${BUILD_DIR}/${CROSSPLANE_VERSION}/charts repo
  RUN --push --secret=AWS_ACCESS_KEY_ID --secret=AWS_SECRET_ACCESS_KEY aws s3 sync --delete --only-show-errors s3://${BUCKET_RELEASES}/build/${BUILD_DIR}/${CROSSPLANE_VERSION} s3://${BUCKET_RELEASES}/${CHANNEL}/${CROSSPLANE_VERSION}
  IF [ "${PRERELEASE}" = "false" ]
    RUN --push --secret=AWS_ACCESS_KEY_ID --secret=AWS_SECRET_ACCESS_KEY aws s3 sync --delete --only-show-errors s3://${BUCKET_RELEASES}/build/${BUILD_DIR}/${CROSSPLANE_VERSION} s3://${BUCKET_RELEASES}/${CHANNEL}/current
  END
