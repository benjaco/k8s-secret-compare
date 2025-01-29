# How to Use

The k8s-secret-compare tool allows you to compare local Kubernetes Secret YAML files (stringData) with the deployed secrets (data) in your Kubernetes cluster. 

It reads all "*secret*.yaml" and "*secret*.yml", and fetches it by the defined namespace and name, pattern can be defined as an arg `secret-compare -pattern="*.yaml"`

## Eg

Processing file: kube-secret-staging.yaml
```
=== kube-secret-staging.yaml ===
Differences found:
- [DIFFERENT] DISABLE_TIMING_LOGS:
  Local:     false
  Deployed:  true

Summary: Differences were found in some secrets.
```

## Exit Codes
The secret-compare tool uses exit codes to indicate the result of the comparison:

Exit Code 0:
All secrets match. Indicates success.

Exit Code 1:
Differences were found. Indicates failure

## Install

[Mac Silicon and Windows precompiled here](https://github.com/benjaco/k8s-secret-compare/tags)
