// This package implements a provisioner for Packer that uses
// Chef to provision the remote machine, specifically with chef-solo (that is,
// without a Chef server).
package chefsolo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/mitchellh/packer/common"
	"github.com/mitchellh/packer/packer"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	CookbookPaths       []string `mapstructure:"cookbook_paths"`
	ExecuteCommand      string   `mapstructure:"execute_command"`
	InstallCommand      string   `mapstructure:"install_command"`
	RemoteCookbookPaths []string `mapstructure:"remote_cookbook_paths"`
	Json                map[string]interface{}
	PreventSudo         bool     `mapstructure:"prevent_sudo"`
	RunList             []string `mapstructure:"run_list"`
	SkipInstall         bool     `mapstructure:"skip_install"`
	StagingDir          string   `mapstructure:"staging_directory"`

	tpl *packer.ConfigTemplate
}

type Provisioner struct {
	config Config
}

type ConfigTemplate struct {
	CookbookPaths string
}

type ExecuteTemplate struct {
	ConfigPath string
	JsonPath   string
	Sudo       bool
}

type InstallChefTemplate struct {
	Sudo bool
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	md, err := common.DecodeConfig(&p.config, raws...)
	if err != nil {
		return err
	}

	p.config.tpl, err = packer.NewConfigTemplate()
	if err != nil {
		return err
	}
	p.config.tpl.UserVars = p.config.PackerUserVars

	if p.config.ExecuteCommand == "" {
		p.config.ExecuteCommand = "{{if .Sudo}}sudo {{end}}chef-solo --no-color -c {{.ConfigPath}} -j {{.JsonPath}}"
	}

	if p.config.InstallCommand == "" {
		p.config.InstallCommand = "curl -L https://www.opscode.com/chef/install.sh | {{if .Sudo}}sudo {{end}}bash"
	}

	if p.config.RunList == nil {
		p.config.RunList = make([]string, 0)
	}

	if p.config.StagingDir == "" {
		p.config.StagingDir = "/tmp/packer-chef-solo"
	}

	// Accumulate any errors
	errs := common.CheckUnusedConfig(md)

	templates := map[string]*string{
		"staging_dir": &p.config.StagingDir,
	}

	for n, ptr := range templates {
		var err error
		*ptr, err = p.config.tpl.Process(*ptr, nil)
		if err != nil {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Error processing %s: %s", n, err))
		}
	}

	sliceTemplates := map[string][]string{
		"cookbook_paths":        p.config.CookbookPaths,
		"remote_cookbook_paths": p.config.RemoteCookbookPaths,
		"run_list":              p.config.RunList,
	}

	for n, slice := range sliceTemplates {
		for i, elem := range slice {
			var err error
			slice[i], err = p.config.tpl.Process(elem, nil)
			if err != nil {
				errs = packer.MultiErrorAppend(
					errs, fmt.Errorf("Error processing %s[%d]: %s", n, i, err))
			}
		}
	}

	validates := map[string]*string{
		"execute_command": &p.config.ExecuteCommand,
		"install_command": &p.config.InstallCommand,
	}

	for n, ptr := range validates {
		if err := p.config.tpl.Validate(*ptr); err != nil {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Error parsing %s: %s", n, err))
		}
	}

	for _, path := range p.config.CookbookPaths {
		pFileInfo, err := os.Stat(path)

		if err != nil || !pFileInfo.IsDir() {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Bad cookbook path '%s': %s", path, err))
		}
	}

	// Process the user variables within the JSON and set the JSON.
	// Do this early so that we can validate and show errors.
	p.config.Json, err = p.processJsonUserVars()
	if err != nil {
		errs = packer.MultiErrorAppend(
			errs, fmt.Errorf("Error processing user variables in JSON: %s", err))
	}

	if errs != nil && len(errs.Errors) > 0 {
		return errs
	}

	return nil
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	if !p.config.SkipInstall {
		if err := p.installChef(ui, comm); err != nil {
			return fmt.Errorf("Error installing Chef: %s", err)
		}
	}

	if err := p.createDir(ui, comm, p.config.StagingDir); err != nil {
		return fmt.Errorf("Error creating staging directory: %s", err)
	}

	cookbookPaths := make([]string, 0, len(p.config.CookbookPaths))
	for i, path := range p.config.CookbookPaths {
		targetPath := fmt.Sprintf("%s/cookbooks-%d", p.config.StagingDir, i)
		if err := p.uploadDirectory(ui, comm, targetPath, path); err != nil {
			return fmt.Errorf("Error uploading cookbooks: %s", err)
		}

		cookbookPaths = append(cookbookPaths, targetPath)
	}

	configPath, err := p.createConfig(ui, comm, cookbookPaths)
	if err != nil {
		return fmt.Errorf("Error creating Chef config file: %s", err)
	}

	jsonPath, err := p.createJson(ui, comm)
	if err != nil {
		return fmt.Errorf("Error creating JSON attributes: %s", err)
	}

	if err := p.executeChef(ui, comm, configPath, jsonPath); err != nil {
		return fmt.Errorf("Error executing Chef: %s", err)
	}

	return nil
}

func (p *Provisioner) Cancel() {
	// Just hard quit. It isn't a big deal if what we're doing keeps
	// running on the other side.
	os.Exit(0)
}

func (p *Provisioner) uploadDirectory(ui packer.Ui, comm packer.Communicator, dst string, src string) error {
	if err := p.createDir(ui, comm, dst); err != nil {
		return err
	}

	// Make sure there is a trailing "/" so that the directory isn't
	// created on the other side.
	if src[len(src)-1] != '/' {
		src = src + "/"
	}

	return comm.UploadDir(dst, src, nil)
}

func (p *Provisioner) createConfig(ui packer.Ui, comm packer.Communicator, localCookbooks []string) (string, error) {
	ui.Message("Creating configuration file 'solo.rb'")

	cookbook_paths := make([]string, len(p.config.RemoteCookbookPaths)+len(localCookbooks))
	for i, path := range p.config.RemoteCookbookPaths {
		cookbook_paths[i] = fmt.Sprintf(`"%s"`, path)
	}

	for i, path := range localCookbooks {
		i = len(p.config.RemoteCookbookPaths) + i
		cookbook_paths[i] = fmt.Sprintf(`"%s"`, path)
	}

	configString, err := p.config.tpl.Process(DefaultConfigTemplate, &ConfigTemplate{
		CookbookPaths: strings.Join(cookbook_paths, ","),
	})
	if err != nil {
		return "", err
	}

	remotePath := filepath.Join(p.config.StagingDir, "solo.rb")
	if err := comm.Upload(remotePath, bytes.NewReader([]byte(configString))); err != nil {
		return "", err
	}

	return remotePath, nil
}

func (p *Provisioner) createJson(ui packer.Ui, comm packer.Communicator) (string, error) {
	ui.Message("Creating JSON attribute file")

	jsonData := make(map[string]interface{})

	// Copy the configured JSON
	for k, v := range p.config.Json {
		jsonData[k] = v
	}

	// Set the run list if it was specified
	if len(p.config.RunList) > 0 {
		jsonData["run_list"] = p.config.RunList
	}

	jsonBytes, err := json.MarshalIndent(jsonData, "", "  ")
	if err != nil {
		return "", err
	}

	// Upload the bytes
	remotePath := filepath.Join(p.config.StagingDir, "node.json")
	if err := comm.Upload(remotePath, bytes.NewReader(jsonBytes)); err != nil {
		return "", err
	}

	return remotePath, nil
}

func (p *Provisioner) createDir(ui packer.Ui, comm packer.Communicator, dir string) error {
	ui.Message(fmt.Sprintf("Creating directory: %s", dir))
	cmd := &packer.RemoteCmd{
		Command: fmt.Sprintf("mkdir -p '%s'", dir),
	}

	if err := cmd.StartWithUi(comm, ui); err != nil {
		return err
	}

	if cmd.ExitStatus != 0 {
		return fmt.Errorf("Non-zero exit status.")
	}

	return nil
}

func (p *Provisioner) executeChef(ui packer.Ui, comm packer.Communicator, config string, json string) error {
	command, err := p.config.tpl.Process(p.config.ExecuteCommand, &ExecuteTemplate{
		ConfigPath: config,
		JsonPath:   json,
		Sudo:       !p.config.PreventSudo,
	})
	if err != nil {
		return err
	}

	ui.Message(fmt.Sprintf("Executing Chef: %s", command))

	cmd := &packer.RemoteCmd{
		Command: command,
	}

	if err := cmd.StartWithUi(comm, ui); err != nil {
		return err
	}

	if cmd.ExitStatus != 0 {
		return fmt.Errorf("Non-zero exit status: %d", cmd.ExitStatus)
	}

	return nil
}

func (p *Provisioner) installChef(ui packer.Ui, comm packer.Communicator) error {
	ui.Message("Installing Chef...")

	command, err := p.config.tpl.Process(p.config.InstallCommand, &InstallChefTemplate{
		Sudo: !p.config.PreventSudo,
	})
	if err != nil {
		return err
	}

	cmd := &packer.RemoteCmd{Command: command}
	if err := cmd.StartWithUi(comm, ui); err != nil {
		return err
	}

	if cmd.ExitStatus != 0 {
		return fmt.Errorf(
			"Install script exited with non-zero exit status %d", cmd.ExitStatus)
	}

	return nil
}

func (p *Provisioner) processJsonUserVars() (map[string]interface{}, error) {
	jsonBytes, err := json.Marshal(p.config.Json)
	if err != nil {
		// This really shouldn't happen since we literally just unmarshalled
		panic(err)
	}

	// Copy the user variables so that we can restore them later, and
	// make sure we make the quotes JSON-friendly in the user variables.
	originalUserVars := make(map[string]string)
	for k, v := range p.config.tpl.UserVars {
		originalUserVars[k] = v
	}

	// Make sure we reset them no matter what
	defer func() {
		p.config.tpl.UserVars = originalUserVars
	}()

	// Make the current user variables JSON string safe.
	for k, v := range p.config.tpl.UserVars {
		v = strings.Replace(v, `\`, `\\`, -1)
		v = strings.Replace(v, `"`, `\"`, -1)
		p.config.tpl.UserVars[k] = v
	}

	// Process the bytes with the template processor
	jsonBytesProcessed, err := p.config.tpl.Process(string(jsonBytes), nil)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(jsonBytesProcessed), &result); err != nil {
		return nil, err
	}

	return result, nil
}

var DefaultConfigTemplate = `
cookbook_path [{{.CookbookPaths}}]
`
