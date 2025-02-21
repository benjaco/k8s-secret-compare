package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1" // Renamed for clarity
	"k8s.io/client-go/kubernetes"
	// Uncomment the following line if you need to use in-cluster config
	// "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// KubernetesSecret represents the structure of a Kubernetes Secret YAML file
type KubernetesSecret struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   Metadata          `yaml:"metadata"`
	Type       string            `yaml:"type,omitempty"`
	StringData map[string]string `yaml:"stringData,omitempty"`
}

// KubernetesConfig represents the structure of a Kubernetes ConfigMap YAML file
type KubernetesConfig struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   Metadata          `yaml:"metadata"`
	Data       map[string]string `yaml:"data,omitempty"`
}

// Metadata holds the metadata information for Kubernetes resources
type Metadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// DeployedData represents the structure of a deployed Kubernetes Secret or ConfigMap
type DeployedData struct {
	Type      string
	Name      string
	Namespace string
	Data      map[string]string
}

// SecretDifference represents a difference in a key-value pair
type SecretDifference struct {
	Key      string
	Local    *string
	Deployed *string
}

// LocalResource is an interface to unify local Secrets and ConfigMaps.
type LocalResource interface {
	GetName() string
	GetNamespace() string
	GetKind() string
	GetLocalData() map[string]string
	GetMergeField() string // "stringData" for Secrets; "data" for ConfigMaps.
}

// Implement LocalResource for KubernetesSecret.
func (s *KubernetesSecret) GetName() string                 { return s.Metadata.Name }
func (s *KubernetesSecret) GetNamespace() string            { return s.Metadata.Namespace }
func (s *KubernetesSecret) GetKind() string                 { return s.Kind }
func (s *KubernetesSecret) GetLocalData() map[string]string { return s.StringData }
func (s *KubernetesSecret) GetMergeField() string           { return "stringData" }

// Implement LocalResource for KubernetesConfig.
func (c *KubernetesConfig) GetName() string                 { return c.Metadata.Name }
func (c *KubernetesConfig) GetNamespace() string            { return c.Metadata.Namespace }
func (c *KubernetesConfig) GetKind() string                 { return c.Kind }
func (c *KubernetesConfig) GetLocalData() map[string]string { return c.Data }
func (c *KubernetesConfig) GetMergeField() string           { return "data" }

func main() {
	// Define command-line flags
	dirPtr := flag.String("dir", ".", "Directory to scan for config and secret YAML files")
	patternPtr := flag.String("pattern", "*secret*.yaml,*secret*.yml,*config*.yaml,*config*.yml", "Comma-separated glob patterns to identify secret & config YAML files (e.g., \"*secret*.yaml,*secret*.yml\")")
	verbosePtr := flag.Bool("verbose", false, "Enable verbose logging")
	flag.Parse()

	// Set up logging
	if *verbosePtr {
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	} else {
		log.SetFlags(0)
	}
	log.SetOutput(os.Stdout)

	// Create Kubernetes client
	clientset, err := getKubernetesClient()
	if err != nil {
		log.Fatalf("Failed to create Kubernetes client: %v", err)
	}

	// Process file patterns
	patterns := parsePatterns(*patternPtr, *dirPtr)
	var files []string
	for _, pattern := range patterns {
		matchedFiles, err := filepath.Glob(pattern)
		if err != nil {
			log.Fatalf("Error processing pattern '%s': %v", pattern, err)
		}
		files = append(files, matchedFiles...)
	}

	if len(files) == 0 {
		log.Println("No YAML files matching the specified patterns were found in the directory.")
		return
	}

	// Variable to track if any differences were found across all files
	var globalDifferencesFound bool = false

	for _, file := range files {
		log.Printf("Processing file: %s\n", filepath.Base(file))
		localResources, err := parseYAMLResources(file)
		if err != nil {
			log.Printf("Error parsing YAML file '%s': %v\n", filepath.Base(file), err)
			continue
		}

		// Process each local resource
		for _, resource := range localResources {
			var deployed *DeployedData
			switch resource.GetKind() {
			case "Secret":
				deployed, err = getDeployedSecret(clientset, resource.GetNamespace(), resource.GetName())
			case "ConfigMap":
				deployed, err = getDeployedConfig(clientset, resource.GetNamespace(), resource.GetName())
			default:
				log.Printf("Skipping unsupported resource type: %s\n", resource.GetKind())
				continue
			}
			if err != nil {
				log.Printf("Error retrieving deployed %s '%s' in namespace '%s': %v\n", resource.GetKind(), resource.GetName(), resource.GetNamespace(), err)
				continue
			}
			if deployed == nil {
				log.Printf("Deployed %s '%s' in namespace '%s' not found.\n", resource.GetKind(), resource.GetName(), resource.GetNamespace())
				continue
			}

			// Use unified comparison logic.
			differences := compareData(resource.GetLocalData(), deployed.Data)
			printDifferences(resource.GetKind(), resource.GetName(), resource.GetNamespace(), differences, resource.GetMergeField(), &globalDifferencesFound)
		}
	}

	// Set exit code based on whether any differences were found
	if globalDifferencesFound {
		fmt.Println("Summary: Differences were found in some resources.")
		os.Exit(1) // Indicates failure due to differences
	} else {
		fmt.Println("Summary: All secrets match across environments.")
		os.Exit(0) // Indicates success
	}
}

// parseYAMLResources reads and parses a YAML file that may contain multiple documents,
// returning a slice of LocalResource (either a KubernetesSecret or KubernetesConfig).
func parseYAMLResources(filePath string) ([]LocalResource, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	var resources []LocalResource

	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error decoding YAML: %w", err)
		}

		// Read the "kind" field to decide how to decode.
		var meta struct {
			Kind string `yaml:"kind"`
		}
		if err := node.Decode(&meta); err != nil {
			log.Printf("Skipping document in file '%s': %v", filepath.Base(filePath), err)
			continue
		}

		switch meta.Kind {
		case "Secret":
			var secret KubernetesSecret
			if err := node.Decode(&secret); err != nil {
				log.Printf("Error decoding Secret in file '%s': %v", filepath.Base(filePath), err)
				continue
			}
			// Validate required fields.
			if secret.Metadata.Name == "" {
				log.Printf("Skipping Secret with missing name  in file '%s'\n", filepath.Base(filePath))
				continue
			}
			// Validate required fields.
			if secret.Metadata.Namespace == "" {
				log.Printf("Skipping Secret with missing namespace in file '%s'\n", filepath.Base(filePath))
				continue
			}
			if len(secret.StringData) == 0 {
				log.Printf("Skipping Secret '%s' in namespace '%s' with no 'stringData' in file '%s'\n", secret.Metadata.Name, secret.Metadata.Namespace, filepath.Base(filePath))
				continue
			}
			resources = append(resources, &secret)
		case "ConfigMap":
			var config KubernetesConfig
			if err := node.Decode(&config); err != nil {
				log.Printf("Error decoding ConfigMap in file '%s': %v", filepath.Base(filePath), err)
				continue
			}
			// Validate required fields.
			if config.Metadata.Name == "" {
				log.Printf("Skipping ConfigMap with missing name in file '%s'\n", filepath.Base(filePath))
				continue
			}
			// Validate required fields.
			if config.Metadata.Namespace == "" {
				log.Printf("Skipping ConfigMap with missing namespace in file '%s'\n", filepath.Base(filePath))
				continue
			}
			if len(config.Data) == 0 {
				log.Printf("Skipping ConfigMap '%s' in namespace '%s' with no 'data' in file '%s'\n", config.Metadata.Name, config.Metadata.Namespace, filepath.Base(filePath))
				continue
			}
			resources = append(resources, &config)
		default:
			log.Printf("Skipping unsupported kind: %s in file '%s'\n", meta.Kind, filepath.Base(filePath))
			continue
		}
	}

	return resources, nil
}

// parsePatterns processes the provided pattern string and returns a slice of glob patterns
func parsePatterns(patternStr, dir string) []string {
	var patterns []string
	rawPatterns := strings.Split(patternStr, ",")
	for _, p := range rawPatterns {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			fullPattern := filepath.Join(dir, trimmed)
			patterns = append(patterns, fullPattern)
		}
	}
	return patterns
}

// getKubernetesClient initializes and returns a Kubernetes clientset
func getKubernetesClient() (*kubernetes.Clientset, error) {
	// Use the current context in kubeconfig
	kubeconfigPath := filepath.Join(homeDir(), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error building kubeconfig: %w", err)
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes client: %w", err)
	}

	return clientset, nil
}

// getDeployedSecret retrieves a deployed Kubernetes Secret from the cluster
func getDeployedSecret(clientset *kubernetes.Clientset, namespace, name string) (*DeployedData, error) {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Secret does not exist in the deployed cluster
			return nil, nil
		}
		return nil, fmt.Errorf("error fetching secret: %w", err)
	}

	// Since client-go decodes 'data', we can directly use it
	decodedData := make(map[string]string)
	for key, value := range secret.Data {
		decodedData[key] = string(value)
	}

	// Note: Typically, 'stringData' is not stored in the deployed secret,
	// as it's mainly used for creating secrets via YAML. However, we'll include it if present.
	for key, value := range secret.StringData {
		decodedData[key] = value
	}

	return &DeployedData{
		Type:      "secret",
		Name:      secret.Name,
		Namespace: secret.Namespace,
		Data:      decodedData,
	}, nil
}

// getDeployedConfig retrieves a deployed Kubernetes ConfigMap from the cluster
func getDeployedConfig(clientset *kubernetes.Clientset, namespace, name string) (*DeployedData, error) {
	config, err := clientset.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Secret does not exist in the deployed cluster
			return nil, nil
		}
		return nil, fmt.Errorf("error fetching configmap: %w", err)
	}

	return &DeployedData{
		Type:      "configmap",
		Name:      config.Name,
		Namespace: config.Namespace,
		Data:      config.Data,
	}, nil
}

// compareData compares the local data with the deployed data and returns differences
func compareData(local, deployed map[string]string) []SecretDifference {
	var differences []SecretDifference

	// Create a set of all keys
	keysSet := make(map[string]struct{})
	for key := range local {
		keysSet[key] = struct{}{}
	}
	for key := range deployed {
		keysSet[key] = struct{}{}
	}

	for key := range keysSet {
		localVal, localExists := local[key]
		deployedVal, deployedExists := deployed[key]

		if !localExists && deployedExists {
			diff := SecretDifference{
				Key:      key,
				Local:    nil,
				Deployed: &deployedVal,
			}
			differences = append(differences, diff)
		} else if localExists && !deployedExists {
			diff := SecretDifference{
				Key:      key,
				Local:    &localVal,
				Deployed: nil,
			}
			differences = append(differences, diff)
		} else if localExists && deployedExists && localVal != deployedVal {
			diff := SecretDifference{
				Key:      key,
				Local:    &localVal,
				Deployed: &deployedVal,
			}
			differences = append(differences, diff)
		}
	}

	return differences
}

// printDifferences prints the comparison results and outputs YAML snippets
// for key-value pairs that should be merged locally.
func printDifferences(kind, name, namespace string, differences []SecretDifference, mergeField string, globalDiffFound *bool) {
	if len(differences) == 0 {
		fmt.Printf("=== %s (Namespace: %s) ===\nAll %s match between the local file and the deployed Kubernetes %s.\n\n", name, namespace, kind, kind)
	} else {
		*globalDiffFound = true
		fmt.Printf("=== %s (Namespace: %s) ===\nDifferences found:\n", name, namespace)

		missingLocalKeys := make(map[string]string)
		replaceLocalKeys := make(map[string]string)

		for _, diff := range differences {
			switch {
			case diff.Local != nil && diff.Deployed != nil:
				fmt.Printf(" - [DIFFERENT] %s:\n", diff.Key)
				fmt.Printf("   Local:     %s\n", *diff.Local)
				fmt.Printf("   Deployed:  %s\n\n", *diff.Deployed)
				replaceLocalKeys[diff.Key] = *diff.Deployed
			case diff.Local != nil && diff.Deployed == nil:
				fmt.Printf(" - [ONLY IN LOCAL] %s:\n", diff.Key)
				fmt.Printf("   Value: %s\n\n", *diff.Local)
			case diff.Local == nil && diff.Deployed != nil:
				fmt.Printf(" - [ONLY IN DEPLOYED] %s:\n", diff.Key)
				fmt.Printf("   Value: %s\n\n", *diff.Deployed)
				missingLocalKeys[diff.Key] = *diff.Deployed
			}
		}

		// the new locals doenst need a copy snippet as is can de applied as it is
		if len(replaceLocalKeys) > 0 {
			fmt.Printf("Merge the following key-value pairs into your local file to match deployed %s:\n", strings.ToLower(kind))
			fmt.Println("```yaml")
			fmt.Printf("%s:\n", mergeField)
			for key, value := range replaceLocalKeys {
				fmt.Printf("  %s: %s\n", key, formatYAMLValue(value))
			}
			fmt.Println("```")
			fmt.Println()
		}
		if len(missingLocalKeys) > 0 {
			fmt.Printf("Add the following key-value pairs locally to match the deployed %s:\n", strings.ToLower(kind))
			fmt.Println("```yaml")
			fmt.Printf("%s:\n", mergeField)
			for key, value := range missingLocalKeys {
				fmt.Printf("  %s: %s\n", key, formatYAMLValue(value))
			}
			fmt.Println("```")
			fmt.Println()
		}
	}
}

// formatYAMLValue formats the value based on whether it's multiline.
// If multiline, it uses the |- indicator; otherwise, it quotes the value.
func formatYAMLValue(value string) string {
	if strings.Contains(value, "\n") {
		// Use |- for multiline strings
		// Indent each line by 4 spaces
		lines := strings.Split(value, "\n")
		var formattedLines []string
		for _, line := range lines {
			formattedLines = append(formattedLines, "    "+line)
		}
		return "|-\n" + strings.Join(formattedLines, "\n")
	}
	// For single-line strings, quote the value
	escapedValue := strings.ReplaceAll(value, "\"", "\\\"") // Escape double quotes
	return fmt.Sprintf("\"%s\"", escapedValue)
}

// homeDir returns the home directory for the current user
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // Windows
}
