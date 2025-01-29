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

// Metadata holds the metadata information for Kubernetes resources
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

		// Parse the YAML file (supports multiple secrets per file)
		localSecrets, err := parseYAMLSecrets(file)
		if err != nil {
			log.Printf("Error parsing YAML file '%s': %v\n", filepath.Base(file), err)
			continue
		}

		for _, localSecret := range localSecrets {
			log.Printf("Comparing Secret: %s in Namespace: %s . . . \n", localSecret.Metadata.Name, localSecret.Metadata.Namespace)

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
				fmt.Printf("=== %s (Namespace: %s) ===\nAll secrets match between the local file and the deployed Kubernetes secret.\n\n", localSecret.Metadata.Name, localSecret.Metadata.Namespace)
			} else {
				globalDifferencesFound = true
				fmt.Printf("=== %s (Namespace: %s) ===\nDifferences found:\n", localSecret.Metadata.Name, localSecret.Metadata.Namespace)

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
					fmt.Println("Merge following key-value pairs into your local file to match deployed secrets:")
					fmt.Println("```yaml")
					fmt.Println("stringData:")
					for key, value := range replaceLocalKeys {
						fmt.Printf("  %s: %s\n", key, formatYAMLValue(value))
					}
					fmt.Println("```")
					fmt.Println()
				}
				if len(missingLocalKeys) > 0 {
					fmt.Println("Following should be added locally to make it match the deployed secrets:")
					fmt.Println("```yaml")
					fmt.Println("stringData:")
					for key, value := range missingLocalKeys {
						fmt.Printf("  %s: %s\n", key, formatYAMLValue(value))
					}
					fmt.Println("```")
					fmt.Println()
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

// parseYAMLSecrets reads and parses a Kubernetes Secret YAML file, supporting multiple secrets per file
func parseYAMLSecrets(filePath string) ([]KubernetesSecret, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	secrets := []KubernetesSecret{}

	for {
		var secret KubernetesSecret
		err := decoder.Decode(&secret)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("error unmarshaling YAML: %w", err)
		}

		// Ensure required fields are present
		if secret.Kind != "Secret" {
			log.Printf("Skipping non-Secret kind: %s in file '%s'\n", secret.Kind, filepath.Base(filePath))
			continue
		}
		if secret.Metadata.Name == "" {
			log.Printf("Skipping secret with missing name in file '%s'\n", filepath.Base(filePath))
			continue
		}
		if secret.Metadata.Namespace == "" {
			log.Printf("Skipping secret with missing namespace in file '%s'\n", filepath.Base(filePath))
			continue
		}

		if len(secret.StringData) == 0 {
			log.Printf("Skipping secret '%s' in namespace '%s' with no 'stringData' in file '%s'\n", secret.Metadata.Name, secret.Metadata.Namespace, filepath.Base(filePath))
			continue
		}

		secrets = append(secrets, secret)
	}

	return secrets, nil
}

// getDeployedSecret retrieves a deployed Kubernetes Secret from the cluster
func getDeployedSecret(clientset *kubernetes.Clientset, namespace, name string) (*DeployedSecret, error) {
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

	return &DeployedSecret{
		Name:      secret.Name,
		Namespace: secret.Namespace,
		Data:      decodedData,
	}, nil
}

// compareSecrets compares local stringData with deployed data and returns differences
func compareSecrets(local KubernetesSecret, deployed *DeployedSecret) []SecretDifference {
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
