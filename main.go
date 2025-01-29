package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
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
	StringData map[string]string `yaml:"stringData,omitempty"`
}

type Metadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// DeployedSecret represents the structure of a deployed Kubernetes Secret
type DeployedSecret struct {
	Name      string
	Namespace string
	Data      map[string]string
}

// SecretDifference represents a difference in a secret's key-value pair
type SecretDifference struct {
	Key      string
	Local    *string
	Deployed *string
}

func main() {
	// Define command-line flags
	dirPtr := flag.String("dir", ".", "Directory to scan for secret YAML files")
	patternPtr := flag.String("pattern", "*secret*.yaml,*secret*.yml", "Comma-separated glob patterns to identify secret YAML files (e.g., \"*secret*.yaml,*secret*.yml\")")
	verbosePtr := flag.Bool("verbose", false, "Enable verbose logging")
	flag.Parse()

	// Set up logging to terminal only
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
	files := []string{}
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

		// Parse the YAML file
		localSecret, err := parseYAMLSecret(file)
		if err != nil {
			log.Printf("Error parsing YAML file '%s': %v\n", filepath.Base(file), err)
			continue
		}

		// Retrieve the deployed secret from Kubernetes
		deployedSecret, err := getDeployedSecret(clientset, localSecret.Metadata.Namespace, localSecret.Metadata.Name)
		if err != nil {
			log.Printf("Error retrieving deployed secret '%s' in namespace '%s': %v\n", localSecret.Metadata.Name, localSecret.Metadata.Namespace, err)
			continue
		}

		// Compare secrets
		differences := compareSecrets(localSecret, deployedSecret)

		// Report differences
		if len(differences) == 0 {
			fmt.Printf("=== %s ===\nAll secrets match between the local file and the deployed Kubernetes secret.\n\n", filepath.Base(file))
		} else {
			globalDifferencesFound = true
			fmt.Printf("=== %s ===\nDifferences found:\n", filepath.Base(file))
			for _, diff := range differences {
				switch {
				case diff.Local != nil && diff.Deployed != nil:
					fmt.Printf(" - [DIFFERENT] %s:\n", diff.Key)
					fmt.Printf("   Local:     %s\n", *diff.Local)
					fmt.Printf("   Deployed:  %s\n\n", *diff.Deployed)
				case diff.Local != nil && diff.Deployed == nil:
					fmt.Printf(" - [ONLY IN LOCAL] %s:\n", diff.Key)
					fmt.Printf("   Value: %s\n\n", *diff.Local)
				case diff.Local == nil && diff.Deployed != nil:
					fmt.Printf(" - [ONLY IN DEPLOYED] %s:\n", diff.Key)
					fmt.Printf("   Value: %s\n\n", *diff.Deployed)
				}
			}
		}
	}

	// Set exit code based on whether any differences were found
	if globalDifferencesFound {
		fmt.Println("Summary: Differences were found in some secrets.")
		os.Exit(1) // Indicates failure due to differences
	} else {
		fmt.Println("Summary: All secrets match across environments.")
		os.Exit(0) // Indicates success
	}
}

// parsePatterns processes the provided pattern string and returns a slice of glob patterns
func parsePatterns(patternStr, dir string) []string {
	patterns := []string{}
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

// parseYAMLSecret reads and parses a Kubernetes Secret YAML file
func parseYAMLSecret(filePath string) (*KubernetesSecret, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	var secret KubernetesSecret
	err = yaml.Unmarshal(data, &secret)
	if err != nil {
		return nil, fmt.Errorf("error unmarshaling YAML: %w", err)
	}

	// Ensure required fields are present
	if secret.Kind != "Secret" {
		return nil, fmt.Errorf("file '%s' is not a Kubernetes Secret", filepath.Base(filePath))
	}
	if secret.Metadata.Name == "" {
		return nil, fmt.Errorf("secret name is missing in file '%s'", filepath.Base(filePath))
	}
	if secret.Metadata.Namespace == "" {
		return nil, fmt.Errorf("namespace is missing in file '%s'", filepath.Base(filePath))
	}

	if len(secret.StringData) == 0 {
		return nil, fmt.Errorf("no 'stringData' found in file '%s'", filepath.Base(filePath))
	}

	return &secret, nil
}

// getDeployedSecret retrieves a deployed Kubernetes Secret from the cluster
func getDeployedSecret(clientset *kubernetes.Clientset, namespace, name string) (*DeployedSecret, error) {
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), name, v1.GetOptions{})
	if err != nil {
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

	return &DeployedSecret{
		Name:      secret.Name,
		Namespace: secret.Namespace,
		Data:      decodedData,
	}, nil
}

// compareSecrets compares local stringData with deployed data and returns differences
func compareSecrets(local *KubernetesSecret, deployed *DeployedSecret) []SecretDifference {
	differences := []SecretDifference{}

	// Create a set of all keys
	keysSet := make(map[string]struct{})
	for key := range local.StringData {
		keysSet[key] = struct{}{}
	}
	for key := range deployed.Data {
		keysSet[key] = struct{}{}
	}

	for key := range keysSet {
		localVal, localExists := local.StringData[key]
		deployedVal, deployedExists := deployed.Data[key]

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

// homeDir returns the home directory for the current user
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // Windows
}
