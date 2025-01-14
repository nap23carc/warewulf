package warewulfd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	warewulfconf "github.com/warewulf/warewulf/internal/pkg/config"
	"github.com/warewulf/warewulf/internal/pkg/container"
	"github.com/warewulf/warewulf/internal/pkg/kernel"
	"github.com/warewulf/warewulf/internal/pkg/node"
	"github.com/warewulf/warewulf/internal/pkg/overlay"
	"github.com/warewulf/warewulf/internal/pkg/util"
	"github.com/warewulf/warewulf/internal/pkg/wwlog"
)

type templateVars struct {
	Message        string
	WaitTime       string
	Hostname       string
	Fqdn           string
	Id             string
	Cluster        string
	ContainerName  string
	Hwaddr         string
	Ipaddr         string
	Port           string
	KernelArgs     string
	KernelOverride string
	Tags           map[string]string
	NetDevs        map[string]*node.NetDevs
}

func ProvisionSend(w http.ResponseWriter, req *http.Request) {
	wwlog.Debug("Requested URL: %s", req.URL.String())
	conf := warewulfconf.Get()
	rinfo, err := parseReq(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		wwlog.ErrorExc(err, "Bad status")
		return
	}

	wwlog.Recv("hwaddr: %s, ipaddr: %s, stage: %s", rinfo.hwaddr, req.RemoteAddr, rinfo.stage)

	if (rinfo.stage == "runtime" || len(rinfo.overlay) > 0) && conf.Warewulf.Secure {
		if rinfo.remoteport >= 1024 {
			wwlog.Denied("Non-privileged port: %s", req.RemoteAddr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	status_stages := map[string]string{
		"efiboot":   "EFI",
		"ipxe":      "IPXE",
		"kernel":    "KERNEL",
		"kmods":     "KMODS_OVERLAY",
		"system":    "SYSTEM_OVERLAY",
		"runtime":   "RUNTIME_OVERLAY",
		"initramfs": "INITRAMFS"}

	status_stage := status_stages[rinfo.stage]
	var stage_file string

	// TODO: when module version is upgraded to go1.18, should be 'any' type
	var tmpl_data interface{}

	remoteNode, err := GetNodeOrSetDiscoverable(rinfo.hwaddr)
	if err != nil && err != node.ErrNoUnconfigured {
		wwlog.ErrorExc(err, "")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	if remoteNode.AssetKey != "" && remoteNode.AssetKey != rinfo.assetkey {
		w.WriteHeader(http.StatusUnauthorized)
		wwlog.Denied("Incorrect asset key for node: %s", remoteNode.Id())
		updateStatus(remoteNode.Id(), status_stage, "BAD_ASSET", rinfo.ipaddr)
		return
	}

	if !remoteNode.Valid() {
		wwlog.Error("%s (unknown/unconfigured node)", rinfo.hwaddr)
		if rinfo.stage == "ipxe" {
			stage_file = path.Join(conf.Paths.Sysconfdir, "/warewulf/ipxe/unconfigured.ipxe")
			tmpl_data = templateVars{
				Hwaddr: rinfo.hwaddr}
		}

	} else if rinfo.stage == "ipxe" {
		stage_file = path.Join(conf.Paths.Sysconfdir, "warewulf/ipxe/"+remoteNode.Ipxe+".ipxe")
		tmpl_data = templateVars{
			Id:             remoteNode.Id(),
			Cluster:        remoteNode.ClusterName,
			Fqdn:           remoteNode.Id(),
			Ipaddr:         conf.Ipaddr,
			Port:           strconv.Itoa(conf.Warewulf.Port),
			Hostname:       remoteNode.Id(),
			Hwaddr:         rinfo.hwaddr,
			ContainerName:  remoteNode.ContainerName,
			KernelArgs:     remoteNode.Kernel.Args,
			KernelOverride: remoteNode.Kernel.Override,
			NetDevs:        remoteNode.NetDevs,
			Tags:           remoteNode.Tags}
	} else if rinfo.stage == "kernel" {
		if remoteNode.Kernel.Override != "" {
			stage_file = kernel.KernelImage(remoteNode.Kernel.Override)
		} else if remoteNode.ContainerName != "" {
			stage_file, _, err = kernel.FindKernel(container.RootFsDir(remoteNode.ContainerName))
			if err != nil {
				wwlog.Error("No kernel found for container %s: %s", remoteNode.ContainerName, err)
			}
		} else {
			wwlog.Warn("No kernel version set for node %s", remoteNode.Id())
		}

	} else if rinfo.stage == "kmods" {
		if remoteNode.Kernel.Override != "" {
			stage_file = kernel.KmodsImage(remoteNode.Kernel.Override)
		} else {
			wwlog.Warn("No kernel override modules set for node %s", remoteNode.Id())
		}

	} else if rinfo.stage == "container" {
		if remoteNode.ContainerName != "" {
			stage_file = container.ImageFile(remoteNode.ContainerName)
		} else {
			wwlog.Warn("No container set for node %s", remoteNode.Id())
		}

	} else if rinfo.stage == "system" || rinfo.stage == "runtime" {
		var context string
		var request_overlays []string

		if len(rinfo.overlay) > 0 {
			request_overlays = strings.Split(rinfo.overlay, ",")
		} else {
			context = rinfo.stage
		}
		stage_file, err = getOverlayFile(
			remoteNode,
			context,
			request_overlays,
			conf.Warewulf.AutobuildOverlays)

		if err != nil {
			if errors.Is(err, overlay.ErrDoesNotExist) {
				w.WriteHeader(http.StatusNotFound)
				wwlog.ErrorExc(err, "")
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			wwlog.ErrorExc(err, "")
			return
		}
	} else if rinfo.stage == "efiboot" {
		wwlog.Debug("requested method: %s", req.Method)
		containerName := remoteNode.ContainerName
		switch rinfo.efifile {
		case "shim.efi":
			stage_file = container.ShimFind(containerName)
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find shim.efi"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case "grub.efi", "grub-tpm.efi", "grubx64.efi", "grubia32.efi", "grubaa64.efi", "grubarm.efi":
			stage_file = container.GrubFind(containerName)
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find grub.efi"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case "grub.cfg":
			stage_file = path.Join(conf.Paths.Sysconfdir, "warewulf/grub/grub.cfg.ww")
			tmpl_data = templateVars{
				Id:             remoteNode.Id(),
				Cluster:        remoteNode.ClusterName,
				Fqdn:           remoteNode.Id(),
				Ipaddr:         conf.Ipaddr,
				Port:           strconv.Itoa(conf.Warewulf.Port),
				Hostname:       remoteNode.Id(),
				Hwaddr:         rinfo.hwaddr,
				ContainerName:  remoteNode.ContainerName,
				KernelArgs:     remoteNode.Kernel.Args,
				KernelOverride: remoteNode.Kernel.Override,
				NetDevs:        remoteNode.NetDevs,
				Tags:           remoteNode.Tags}
			if stage_file == "" {
				wwlog.ErrorExc(fmt.Errorf("could't find grub.cfg template"), containerName)
				w.WriteHeader(http.StatusNotFound)
				return
			}
		default:
			wwlog.ErrorExc(fmt.Errorf("could't find efiboot file: %s", rinfo.efifile), "")
		}
	} else if rinfo.stage == "shim" {
		if remoteNode.ContainerName != "" {
			stage_file = container.ShimFind(remoteNode.ContainerName)

			if stage_file == "" {
				wwlog.Error("No kernel found for container %s", remoteNode.ContainerName)
			}
		} else {
			wwlog.Warn("No container set for this %s", remoteNode.Id())
		}
	} else if rinfo.stage == "grub" {
		if remoteNode.ContainerName != "" {
			stage_file = container.GrubFind(remoteNode.ContainerName)
			if stage_file == "" {
				wwlog.Error("No grub found for container %s", remoteNode.ContainerName)
			}
		} else {
			wwlog.Warn("No conainer set for node %s", remoteNode.Id())
		}
	} else if rinfo.stage == "initramfs" {
		if remoteNode.ContainerName != "" {
			_, kver, err := kernel.FindKernel(container.RootFsDir(remoteNode.ContainerName))
			if err != nil {
				wwlog.Error("No kernel found for initramfs for container %s: %s", remoteNode.ContainerName, err)
			}
			stage_file, err = container.InitramfsBootPath(remoteNode.ContainerName, kver)
			if err != nil {
				wwlog.Error("No initramfs found for container %s: %s", remoteNode.ContainerName, err)
			}
		} else {
			wwlog.Warn("No container set for node %s", remoteNode.Id())
		}
	}

	wwlog.Serv("stage_file '%s'", stage_file)

	if util.IsFile(stage_file) {

		if tmpl_data != nil {
			if rinfo.compress != "" {
				wwlog.Error("Unsupported %s compressed version for file: %s",
					rinfo.compress, stage_file)
				w.WriteHeader(http.StatusNotFound)
				return
			}

			// Create a template with the Sprig functions.
			tmpl := template.New(filepath.Base(stage_file)).Funcs(sprig.TxtFuncMap())

			// Parse the template.
			parsedTmpl, err := tmpl.ParseFiles(stage_file)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			// template engine writes file to buffer in case rendering fails
			var buf bytes.Buffer

			err = parsedTmpl.Execute(&buf, tmpl_data)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				wwlog.ErrorExc(err, "")
				return
			}

			w.Header().Set("Content-Type", "text")
			w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
			_, err = buf.WriteTo(w)
			if err != nil {
				wwlog.ErrorExc(err, "")
			}

			wwlog.Send("%15s: %s", remoteNode.Id(), stage_file)

		} else {
			if rinfo.compress == "gz" {
				stage_file += ".gz"

				if !util.IsFile(stage_file) {
					wwlog.Error("unprepared for compressed version of file %s",
						stage_file)
					w.WriteHeader(http.StatusNotFound)
					return
				}
			} else if rinfo.compress != "" {
				wwlog.Error("unsupported %s compressed version of file %s",
					rinfo.compress, stage_file)
				w.WriteHeader(http.StatusNotFound)
			}

			err = sendFile(w, req, stage_file, remoteNode.Id())
			if err != nil {
				wwlog.ErrorExc(err, "")
				return
			}
		}

		updateStatus(remoteNode.Id(), status_stage, path.Base(stage_file), rinfo.ipaddr)

	} else if stage_file == "" {
		w.WriteHeader(http.StatusBadRequest)
		wwlog.Error("No resource selected")
		updateStatus(remoteNode.Id(), status_stage, "BAD_REQUEST", rinfo.ipaddr)

	} else {
		w.WriteHeader(http.StatusNotFound)
		wwlog.Error("Not found: %s", stage_file)
		updateStatus(remoteNode.Id(), status_stage, "NOT_FOUND", rinfo.ipaddr)
	}

}
