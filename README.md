# Standalone Object Storage for Edge Computing
Kubernetes [Operator](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/) that deploys, configure and maintains a fully S3 compliant standalone object storage for edge computing.
## Description
A project that leverages the modularity of Ceph to provide a standalone object storage based on the
Ceph RADOS gateway (RGW), that is not backed by RADOS. This provides both the rich capabilities and
featureset that RGW has (S3 compatibility, security features, multisite, etc.), and also allows it
to run in a lower resources environment and simplifies its deployment which makes it a possible
solution for environments such as Edge. The solution is based on implementing an alternative
librados that allows plugging in different backends, and at first iteration an SQLite based backend.

## Getting Started

For installation, deployment, and administration, see our [Documentation](docs/INSTALL.md).

## Contributing

We welcome contributions. See [Contributing](docs/CONTRIBUTING.md) to get started.

## License

Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
