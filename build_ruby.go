package main

import (
	"archive/tar"
	"bytes"
	"code.google.com/p/go-uuid/uuid"
	"fmt"
	"github.com/codegangsta/cli"
	"github.com/fsouza/go-dockerclient"
	"github.com/wsxiaoys/terminal/color"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"text/template"
)

var (
	distros = map[string]string{
		"ubuntu_precise": "ubuntu:12.04",
		"ubuntu:12.04":   "ubuntu:12.04",

		"ubuntu_raring": "ubuntu:13.04",
		"ubuntu:13.04":  "ubuntu:13.04",

		"ubuntu_trusty": "ubuntu:14.04",
		"ubuntu:14.04":  "ubuntu:14.04",
	}

	docker_client   *docker.Client
	docker_endpoint string
	red             func(string) string
	green           func(string) string
	light_green     func(string) string
)

const image_tag string = "ruby_build"

func init() {
	u, err := url.Parse(os.Getenv("DOCKER_HOST"))
	if err != nil {
		panic(err)
	}
	u.Scheme = "http"
	docker_endpoint = u.String()

	c, err := docker.NewClient(docker_endpoint)
	if err != nil {
		panic(err)
	}
	docker_client = c
}

func main() {

	app := cli.NewApp()
	app.Name = "build_ruby"
	app.Usage = "Build ruby debs from source for Ubuntu"

	app.Flags = []cli.Flag{
		cli.StringFlag{"ruby, r", "", "Required. The version to build, eg. 2.1.0 (for recent versions with no patch release) or 2.0.0-p451"},
		cli.StringFlag{"distro, d", "ubuntu:12.04", "Which distro to use for the build"},
		cli.StringFlag{"arch, a", "amd64", "Arch to use in package filename, eg: 'none', 'all', 'amd64' etc."},
		cli.StringFlag{"iteration, i", "", "eg: 37s~precise"},
	}
	app.Action = buildRuby
	app.Run(os.Args)
}

func buildRuby(c *cli.Context) {
	if c.String("ruby") == "" {
		color.Fprintln(os.Stderr, "@{r!}You didn't specify a Ruby version to build!")
		cli.ShowAppHelp(c)
		os.Exit(1)
	}

	if distros[c.String("distro")] == "" {
		color.Fprintln(os.Stderr, "@{r!}You specified a distro that I don't know how to build for")
		cli.ShowAppHelp(c)
		os.Exit(1)
	}

	var dockerfile *bytes.Buffer = dockerFileFromTemplate(distros[c.String("distro")], c.String("ruby"), c.String("arch"), c.String("iteration"))
	color.Println("@{g!}Using Dockerfile:")
	color.Printf("@{gc}%s\n", dockerfile)
	var build_tarfile *bytes.Buffer = createTarFileFromDockerfile(dockerfile)

	image_name := fmt.Sprintf("%s_%s", "ruby_build", uuid.NewRandom())
	opts := docker.BuildImageOptions{
		Name:           image_name,
		NoCache:        false,
		SuppressOutput: false,
		RmTmpContainer: false,
		InputStream:    build_tarfile,
		OutputStream:   os.Stdout,
	}
	if err := docker_client.BuildImage(opts); err != nil {
		panic(err)
	}
	color.Printf("@{g!}Created image with name %s\n", image_name)

	image, err := docker_client.InspectImage(image_name)
	if err != nil {
		panic(err)
	}

	/*
		Create a container with the created image id

		This seems like a hack. We need a "container" to enable us to copy the ruby
		package out, but I can't see how to do this without needed to run a command
		or just use specify an image ID directly, hence the noop 'date' command.

	*/

	color.Printf("@{g!}Creating container with from image id %s\n", image.ID)
	config := docker.Config{AttachStdout: false, AttachStdin: false, Image: image.ID, Cmd: []string{"date"}}
	create_container_opts := docker.CreateContainerOptions{Name: image.ID, Config: &config}
	container, err := docker_client.CreateContainer(create_container_opts)
	if err != nil {
		panic(err)
	}

	if err := docker_client.StopContainer(container.ID, 1); err != nil {
		panic(err)
	}

	copyPackageFromContainerToLocalFs(container, rubyPackageFileName(c.String("ruby"), c.String("iteration"), c.String("arch")))

	color.Println("@{g!}Removing container:", container.ID)
	if err := docker_client.RemoveContainer(docker.RemoveContainerOptions{ID: container.ID, RemoveVolumes: true, Force: false}); err != nil {
		panic(err)
	}
}

func createTarFileFromDockerfile(dockerfile *bytes.Buffer) *bytes.Buffer {
	// Create a buffer to write our archive to.
	buf := new(bytes.Buffer)

	// Create a new tar archive.
	tw := tar.NewWriter(buf)

	// Add the Dockerfile
	hdr := &tar.Header{
		Name: "Dockerfile",
		Size: int64(dockerfile.Len()),
	}

	if err := tw.WriteHeader(hdr); err != nil {
		panic(err)
	}

	if _, err := tw.Write(dockerfile.Bytes()); err != nil {
		panic(err)
	}

	// Make sure to check the error on Close.
	if err := tw.Close(); err != nil {
		panic(err)
	}

	return buf
}

func copyPackageFromContainerToLocalFs(container *docker.Container, filename string) {
	color.Println("@{g!}Copying package out of the container")

	var buf bytes.Buffer
	if err := docker_client.CopyFromContainer(docker.CopyFromContainerOptions{
		Container:    container.ID,
		Resource:     filename,
		OutputStream: &buf,
	}); err != nil {
		panic(err)
	}

	buffer := bytes.NewReader(buf.Bytes())

	var tar_out *tar.Reader = tar.NewReader(buffer)
	tar_header, err := tar_out.Next()
	if err != nil {
		panic(err)
	}

	color.Printf("@{g!}Extracting pckage file %s (%d bytes)\n", tar_header.Name, tar_header.Size)

	out, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer out.Close()

	io.Copy(out, tar_out)
}

func rubyPackageFileName(version, iteration, arch string) string {
	var formatted_iteration = ""
	if iteration != "" {
		formatted_iteration = "_" + iteration
	}

	var formatted_arch = ""
	if arch != "none" {
		formatted_arch = "_" + arch
	}
	return "ruby-" + version + formatted_iteration + formatted_arch + ".deb"
}

func dockerFileFromTemplate(distro, ruby_version, arch, iteration string) *bytes.Buffer {
	type buildVars struct {
		Distro      string
		RubyVersion string
		Arch        string
		Iteration   string
		DownloadUrl string
		FileName    string
		NumCPU      int
	}

	var formatted_iteration = ""
	if iteration != "" {
		formatted_iteration = fmt.Sprintf("--iteration %s \\", iteration)
	}

	download_url := rubyDownloadUrl(ruby_version)
	dockerfile_vars := buildVars{distro, ruby_version, arch, formatted_iteration, download_url, rubyPackageFileName(ruby_version, iteration, arch), runtime.NumCPU()}

	dockerfile_template, err := Asset("data/Dockerfile.template")
	if err != nil {
		panic(err)
	}
	if len(dockerfile_template) == 0 {
		panic("Couldn't find Dockerfile template in bindata")
	}

	tmpl, err := template.New("dockerfile_template").Parse(string(dockerfile_template))
	if err != nil {
		panic(err)
	}

	buf := new(bytes.Buffer)

	err = tmpl.Execute(buf, dockerfile_vars)
	if err != nil {
		panic(err)
	}

	return buf
}

func rubyDownloadUrl(version string) string {
	// eg:
	// http://cache.ruby-lang.org/pub/ruby/2.1/ruby-2.1.1.tar.gz
	// http://cache.ruby-lang.org/pub/ruby/2.0/ruby-2.0.0-p451.tar.gz

	v := majorMinorVersionOnly(version)
	return "http://cache.ruby-lang.org/pub/ruby/" + v + "/ruby-" + version + ".tar.gz"
}

func majorMinorVersionOnly(full_version string) string {
	return strings.Join(strings.SplitN(full_version, ".", 3)[0:2], ".")
}