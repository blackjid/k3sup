package cmd

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	config "github.com/alexellis/k3sup/pkg/config"
	kssh "github.com/alexellis/k3sup/pkg/ssh"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/terminal"
)

var kubeconfig []byte

func MakeInstall() *cobra.Command {
	var command = &cobra.Command{
		Use:          "install",
		Short:        "Install k3s on a server via SSH",
		Long:         `Install k3s on a server via SSH.`,
		Example:      `  k3sup install --ip 192.168.0.100 --user root`,
		SilenceUsage: true,
	}

	command.Flags().IP("ip", nil, "Public IP of node")
	command.Flags().String("user", "root", "Username for SSH login")

	command.Flags().String("ssh-key", "~/.ssh/id_rsa", "The ssh key to use for remote login")
	command.Flags().Int("ssh-port", 22, "The port on which to connect for ssh")
	command.Flags().Bool("skip-install", false, "Skip the k3s installer")
	command.Flags().String("local-path", "kubeconfig", "Local path to save the kubeconfig file")
	command.Flags().String("k3s-extra-args", "", "Optional extra arguments to pass to k3s installer, wrapped in quotes (e.g. --k3s-extra-args '--no-deploy servicelb')")
	command.Flags().Bool("merge", false, "Merge the config with existing kubeconfig if it already exists.\nProvide the --local-path flag with --merge if a kubeconfig already exists in some other directory")
	command.Flags().String("k3s-version", config.K3sVersion, "Optional version to install, pinned at a default")

	command.RunE = func(command *cobra.Command, args []string) error {

		localKubeconfig, _ := command.Flags().GetString("local-path")

		skipInstall, _ := command.Flags().GetBool("skip-install")

		port, _ := command.Flags().GetInt("ssh-port")

		ip, _ := command.Flags().GetIP("ip")
		fmt.Println("Public IP: " + ip.String())

		user, _ := command.Flags().GetString("user")
		sshKey, _ := command.Flags().GetString("ssh-key")
		merge, _ := command.Flags().GetBool("merge")
		k3sExtraArgs, _ := command.Flags().GetString("k3s-extra-args")

		k3sVersion, _ := command.Flags().GetString("k3s-version")

		sshKeyPath := expandPath(sshKey)
		fmt.Printf("ssh -i %s %s@%s\n", sshKeyPath, user, ip.String())

		authMethod, closeSSHAgent, err := loadPublickey(sshKeyPath)
		if err != nil {
			return errors.Wrapf(err, "unable to load the ssh key with path %q", sshKeyPath)
		}

		defer closeSSHAgent()

		config := &ssh.ClientConfig{
			User: user,
			Auth: []ssh.AuthMethod{
				authMethod,
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}

		address := fmt.Sprintf("%s:%d", ip.String(), port)
		operator, err := kssh.NewSSHOperator(address, config)

		if err != nil {
			return errors.Wrapf(err, "unable to connect to %s over ssh", address)
		}

		defer operator.Close()

		if !skipInstall {
			installK3scommand := fmt.Sprintf("curl -sLS https://get.k3s.io | INSTALL_K3S_EXEC='server --tls-san %s %s' INSTALL_K3S_VERSION='%s' sh -\n", ip, strings.TrimSpace(k3sExtraArgs), k3sVersion)

			fmt.Printf("ssh: %s\n", installK3scommand)
			res, err := operator.Execute(installK3scommand)

			if err != nil {
				return fmt.Errorf("Error received processing command: %s", err)
			}

			fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))
		}

		getConfigcommand := fmt.Sprintf("sudo cat /etc/rancher/k3s/k3s.yaml\n")
		fmt.Printf("ssh: %s\n", getConfigcommand)

		res, err := operator.Execute(getConfigcommand)

		if err != nil {
			return fmt.Errorf("Error received processing command: %s", err)
		}

		fmt.Printf("Result: %s %s\n", string(res.StdOut), string(res.StdErr))

		absPath, _ := filepath.Abs(localKubeconfig)

		kubeconfig := []byte(strings.NewReplacer("localhost", ip.String(), "127.0.0.1", ip.String()).Replace(string(res.StdOut)))

		if merge {
			// Create a merged kubeconfig
			kubeconfig, err = mergeConfigs(absPath, []byte(kubeconfig))
			if err != nil {
				return err
			}
		}

		// Create a new kubeconfig
		if writeErr := writeConfig(absPath, []byte(kubeconfig), false); writeErr != nil {
			return writeErr
		}

		return nil
	}

	command.PreRunE = func(command *cobra.Command, args []string) error {
		_, ipErr := command.Flags().GetIP("ip")
		if ipErr != nil {
			return ipErr
		}

		_, sshPortErr := command.Flags().GetInt("ssh-port")
		if sshPortErr != nil {
			return sshPortErr
		}
		return nil
	}
	return command
}

// Generates config files give the path to file: string and the data: []byte
func writeConfig(path string, data []byte, suppressMessage bool) error {
	absPath, _ := filepath.Abs(path)
	if !suppressMessage {
		fmt.Printf("Saving file to: %s\n", absPath)
	}
	writeErr := ioutil.WriteFile(absPath, []byte(data), 0600)
	if writeErr != nil {
		return writeErr
	}
	return nil
}

func mergeConfigs(localKubeconfigPath string, k3sconfig []byte) ([]byte, error) {
	// Create a temporary kubeconfig to store the config of the newly create k3s cluster
	file, err := ioutil.TempFile(os.TempDir(), "k3s-temp-*")
	if err != nil {
		return nil, fmt.Errorf("Could not generate a temporary file to store the kuebeconfig: %s", err)
	}
	defer file.Close()

	if writeErr := writeConfig(file.Name(), []byte(k3sconfig), true); writeErr != nil {
		return nil, writeErr
	}

	fmt.Printf("Merging with existing kubeconfig at %s\n", localKubeconfigPath)

	// Append KUBECONFIGS in ENV Vars
	appendKubeConfigENV := fmt.Sprintf("KUBECONFIG=%s:%s", localKubeconfigPath, file.Name())

	// Merge the two kubeconfigs and read the output into 'data'
	cmd := exec.Command("kubectl", "config", "view", "--merge", "--flatten")
	cmd.Env = append(os.Environ(), appendKubeConfigENV)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("Could not merge kubeconfigs: %s", err)
	}

	// Remove the temporarily generated file
	err = os.Remove(file.Name())
	if err != nil {
		return nil, errors.Wrapf(err, "Could not remove temporary kubeconfig file: %s", file.Name())
	}

	return data, nil
}

func expandPath(path string) string {
	res, _ := homedir.Expand(path)
	return res
}

func sshAgent(publicKeyPath string) (ssh.AuthMethod, func() error) {
	if sshAgentConn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK")); err == nil {
		sshAgent := agent.NewClient(sshAgentConn)

		keys, _ := sshAgent.List()
		if len(keys) == 0 {
			return nil, sshAgentConn.Close
		}

		pubkey, err := ioutil.ReadFile(publicKeyPath)
		if err != nil {
			return nil, sshAgentConn.Close
		}

		authkey, _, _, _, err := ssh.ParseAuthorizedKey(pubkey)
		if err != nil {
			return nil, sshAgentConn.Close
		}
		parsedkey := authkey.Marshal()

		for _, key := range keys {
			if bytes.Equal(key.Blob, parsedkey) {
				return ssh.PublicKeysCallback(sshAgent.Signers), sshAgentConn.Close
			}
		}
	}
	return nil, func() error { return nil }
}

func loadPublickey(path string) (ssh.AuthMethod, func() error, error) {
	noopCloseFunc := func() error { return nil }

	key, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, noopCloseFunc, err
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		if err.Error() != "ssh: cannot decode encrypted private keys" {
			return nil, noopCloseFunc, err
		}

		agent, close := sshAgent(path + ".pub")
		if agent != nil {
			return agent, close, nil
		}

		defer close()

		fmt.Printf("Enter passphrase for '%s': ", path)
		bytePassword, err := terminal.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()

		signer, err = ssh.ParsePrivateKeyWithPassphrase(key, bytePassword)
		if err != nil {
			return nil, noopCloseFunc, err
		}
	}

	return ssh.PublicKeys(signer), noopCloseFunc, nil
}
