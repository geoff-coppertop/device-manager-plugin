package devicepluginserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"

	"github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/radovskyb/watcher"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	fs "github.com/geoff-coppertop/device-manager-plugin/internal/util/fs"
)

type device struct {
	path   string
	device *pluginapi.Device
}

type deviceMap struct {
	hostPath      string
	containerPath string
}

type DevicePluginServer struct {
	devices     []device
	socket      string
	groupName   string
	mapSymPaths bool
	server      *grpc.Server
	restart     bool
	wg          sync.WaitGroup
	healthUpd   chan bool
	err         chan error
}

// NewDevicePluginServer returns an initialized DevicePluginServer
func NewDevicePluginServer(devicePaths []string, deviceGroupName string, mapSymPaths bool) *DevicePluginServer {
	srv := DevicePluginServer{
		groupName:   deviceGroupName,
		socket:      pluginapi.DevicePluginPath + "dps-" + deviceGroupName + ".sock",
		err:         make(chan error),
		mapSymPaths: mapSymPaths,
	}

	glog.V(2).Infof("%s has %d paths", srv.groupName, len(devicePaths))
	glog.V(3).Infof("Paths: %s", devicePaths)

	for _, path := range devicePaths {
		srv.devices = append(srv.devices, device{
			device: &pluginapi.Device{
				ID:     uuid.NewString(),
				Health: pluginapi.Healthy,
			},
			path: path,
		})
	}

	return &srv
}

// Run makes the plugin go, plugin can be terminated using the provided context
func (m *DevicePluginServer) Run(ctx context.Context, wg *sync.WaitGroup, errCh chan<- error) {
	glog.V(2).Info("Entering Run")

	wg.Add(1)

	go func() {
		defer wg.Done()
		defer m.stop()

		glog.V(2).Info("Starting run thread")

		m.restart = true

		for {
			var restartCtx context.Context
			var restartFunc context.CancelFunc

			if m.restart {
				m.restart = false
				restartCtx, restartFunc = context.WithCancel(ctx)

				glog.V(2).Info("Server starting...")

				err := m.start(restartCtx, restartFunc)
				if err != nil {
					m.err <- err
				}

				glog.V(2).Info("Server started.")
			}

			select {
			case <-restartCtx.Done():
				if m.restart {
					glog.V(2).Info("Restart requested.")
					continue
				}

				glog.V(2).Info("Shutdown requested.")
				err := m.stop()
				if err != nil {
					errCh <- err
				}
				return

			case err := <-m.err:
				glog.Errorf("DPS Error: %v", err)
				errCh <- err
				restartFunc()
				continue
			}
		}

		m.wg.Wait()
		close(m.err)
	}()

	glog.V(2).Info("Server running.")
}

// start sets up kubelet and device watchers and registers the plugin
func (m *DevicePluginServer) start(ctx context.Context, restart context.CancelFunc) error {
	glog.V(2).Info("Cleanup previous runs")

	if err := m.stop(); err != nil {
		return err
	}

	glog.V(2).Info("Starting kubelet watcher")

	if err := m.startKubeletWatcher(ctx, restart); err != nil {
		return err
	}

	glog.V(2).Info("Starting device watcher")

	if err := m.startDeviceWatcher(ctx); err != nil {
		return err
	}

	glog.V(2).Info("Registering with Kubelet")

	if err := m.register(ctx); err != nil {
		return err
	}

	glog.V(2).Info("Device Plugin Server successfully started.")

	return nil
}

// stop shuts down kubelet communication and cleans up
func (m *DevicePluginServer) stop() error {
	if m.server != nil {
		glog.V(2).Info("Stopping socket server")

		m.server.Stop()
		m.server = nil

		glog.V(2).Info("Socket server stopped")
	}

	glog.V(2).Infof("Removing file socket file: %s", m.socket)

	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}

	glog.V(2).Info("Socket file removed")

	return nil
}

// startKubeletWatcher watches the Kubelet socket path for changes and restarts the plugin if add
// or remove is detected
func (m *DevicePluginServer) startKubeletWatcher(ctx context.Context, restart context.CancelFunc) error {
	glog.V(2).Info("Creating kubelet watcher")

	w := watcher.New()
	w.FilterOps(watcher.Create)
	w.SetMaxEvents(1)

	glog.V(2).Info("Kubelet watcher created.")

	err := w.Add(pluginapi.KubeletSocket)
	if err != nil {
		return err
	}

	glog.V(2).Infof("Added watch on, %s", pluginapi.KubeletSocket)

	go func() {
		// Start the watching process - it'll check for changes every 100ms.
		if err := w.Start(time.Millisecond * 100); err != nil {
			m.err <- err
		}
	}()

	glog.V(2).Infof("Started watch on, %s", pluginapi.KubeletSocket)

	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer w.Close()

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		select {
		case _, ok := <-w.Event:
			if !ok {
				return
			}

			glog.V(2).Infof("%s created, restarting.", pluginapi.KubeletSocket)
			m.restart = true
			restart()

		case err, ok := <-w.Error:
			if !ok {
				return
			}

			glog.V(2).Infof("%s, restarting.", err)
			m.restart = true
			restart()

		case <-w.Closed:
			return

		case <-ctx.Done():
			return
		}
	}()

	return nil
}

// startDeviceWatcher starts watching for changes in the device list, the list is immutable once
// started so inotify removed is treated as the device becoming unhealth, added is treated as
// becoming healthy again.
func (m *DevicePluginServer) startDeviceWatcher(ctx context.Context) error {
	glog.V(2).Info("Creating device watcher")

	w := watcher.New()
	w.SetMaxEvents(1)
	w.FilterOps(watcher.Create, watcher.Remove)

	glog.V(2).Info("Device watcher created")

	for _, device := range m.devices {
		path := device.path

		if fs.IsSymlink(path) {
			symPath, err := fs.FollowSymlink(path)
			if err != nil {
				return err
			}

			path = symPath
		}

		glog.V(2).Infof("Adding watch on: %s", path)

		err := w.Add(path)
		if err != nil {
			w.Close()
			return err
		}
	}

	go func() {
		// Start the watching process - it'll check for changes every 100ms.
		if err := w.Start(time.Millisecond * 100); err != nil {
			m.err <- err
		}
	}()

	m.wg.Add(1)

	go func() {
		defer m.wg.Done()
		defer close(m.healthUpd)
		defer w.Close()

		m.healthUpd = make(chan bool)

		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		for {
			select {
			case <-ctx.Done():
				m.healthUpd <- false
				return

			case err, ok := <-w.Error:
				if !ok {
					return
				}

				m.err <- err
				return

			case <-w.Closed:
				return

			case event, ok := <-w.Event:
				if !ok {
					return
				}

				path := event.Path
				op := event.Op

				glog.V(2).Infof("FS watch event for: %s", path)

				var newHealth string

				switch {
				case op&watcher.Create == watcher.Create:
					newHealth = pluginapi.Healthy

				case op&watcher.Remove == watcher.Remove:
					newHealth = pluginapi.Unhealthy

				default:
					glog.V(2).Infof("Device: %s, encountered: %d", path, op)
					continue
				}

				err := m.updateDeviceHealth(newHealth, path)
				if err != nil {
					m.err <- err
					return
				}
			}
		}
	}()

	return nil
}

// register sets up communication between the kubelet and the device plugin for the given
// resourceName
func (m *DevicePluginServer) register(ctx context.Context) error {
	glog.V(2).Infof("Creating plugin socket: %s", m.socket)

	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	glog.V(2).Info("Starting socket server")

	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	pluginapi.RegisterDevicePluginServer(m.server, m)

	go m.server.Serve(sock)

	// Wait for server to start by launching a blocking connexion
	conn, err := m.connect(ctx, m.socket, 60*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	// Try to connect to the kubelet API
	conn, err = m.connect(ctx, pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	resourceName := "device-plugin-server/" + m.groupName

	glog.V(2).Infof("Making device plugin registration request for: %s", resourceName)

	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: resourceName,
	}

	glog.V(2).Info("Registering device plugin")

	_, err = client.Register(ctx, req)
	if err != nil {
		return err
	}

	return nil
}

// connect establishes the gRPC communication with the provided socket path
func (m *DevicePluginServer) connect(ctx context.Context, socketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	glog.V(2).Infof("Connecting to socket: %s", socketPath)

	connCtx, cancel := context.WithDeadline(ctx, time.Now().Add(timeout))
	defer cancel()

	c, err := grpc.DialContext(
		connCtx,
		socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(
			func(ctx context.Context, addr string) (net.Conn, error) {
				var d net.Dialer

				d.LocalAddr = nil
				raddr := net.UnixAddr{Name: addr, Net: "unix"}
				return d.DialContext(ctx, "unix", raddr.String())
			}),
	)

	if err != nil {
		return nil, err
	}

	glog.V(2).Infof("Connected to socket: %s", socketPath)

	return c, nil
}

// getDeviceList returns the device list
func (m *DevicePluginServer) getDeviceList() (devs []*pluginapi.Device) {
	glog.V(2).Infof("There are %d devce(s)", len(m.devices))

	for _, dev := range m.devices {
		devs = append(devs, dev.device)
	}

	glog.V(2).Infof("Found %d devce(s)", len(devs))

	return devs
}

// getDeviceByPath returns a Device struct for the device with the matching path in the device list
func (m *DevicePluginServer) getDeviceByPath(path string) (device, error) {
	for _, dev := range m.devices {
		if dev.path == path {
			glog.V(2).Infof("Found device with path: %s", path)
			return dev, nil
		}
	}

	return device{}, fmt.Errorf("device not found")
}

// getDeviceById returns a Device struct for the device with the matching id in the device list
func (m *DevicePluginServer) getDeviceById(id string) (dev device, err error) {
	for _, d := range m.devices {
		if d.device.ID == id {
			glog.V(2).Infof("Found device with ID: %s", id)
			return d, nil
		}
	}

	return device{}, fmt.Errorf("unknown device: %s", id)
}

// setDeviceHealth returns the health of the device given by path in the device list
func (m *DevicePluginServer) getDeviceHealth(path string) (string, error) {
	dev, err := m.getDeviceByPath(path)
	if err != nil {
		return "", err
	}

	health := dev.device.GetHealth()
	glog.V(2).Infof("Device: %s, is: %s", path, health)
	return health, nil
}

// setDeviceHealth sets the health of the device given by path in the device list
func (m *DevicePluginServer) setDeviceHealth(path string, health string) error {
	if health != pluginapi.Healthy && health != pluginapi.Unhealthy {
		return fmt.Errorf(fmt.Sprintf("unrecognized health: %s", health))
	}

	dev, err := m.getDeviceByPath(path)
	if err != nil {
		return err
	}

	glog.V(2).Infof("Device: %s, now: %s", path, health)
	dev.device.Health = health
	return nil
}

// updateDeviceHealth
func (m *DevicePluginServer) updateDeviceHealth(health string, path string) error {
	var paths []string

	paths = append(paths, path)

	symPath, err := m.findDeviceSymlinkToPath(path)
	if err == nil {
		paths = append(paths, symPath)
	}

	for _, p := range paths {
		initHealth, err := m.getDeviceHealth(p)
		if err != nil {
			return err
		}

		if err := m.setDeviceHealth(p, health); err != nil {
			return err
		}

		glog.V(2).Infof("Device: %s, was: %s, now %s", p, initHealth, health)
	}

	// Notify that there was a health update
	m.healthUpd <- true

	return nil
}

// findDeviceSymlinkToPath returns a device in the list with a symlink that points to the provided
// path, returns an error if no symlink is found
func (m *DevicePluginServer) findDeviceSymlinkToPath(path string) (string, error) {
	for _, dev := range m.devices {
		if fs.IsSymlink(dev.path) {
			symPath, err := fs.FollowSymlink(dev.path)
			if err != nil {
				return "", err
			}

			if symPath == path {
				glog.V(2).Infof("Found symlink %s that points to  %s", dev.path, path)
				return dev.path, nil
			}
		}
	}

	return "", fmt.Errorf("no symlink points to the given path")
}

// API Functions

// GetDevicePluginOptions returns options to be communicated with Device Manager
func (m *DevicePluginServer) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{}, nil
}

// ListAndWatch returns a stream of List of Devices Whenever a Device state change or a Device
// disapears, ListAndWatch returns the new list
func (m *DevicePluginServer) ListAndWatch(empty *pluginapi.Empty, srv pluginapi.DevicePlugin_ListAndWatchServer) error {
	m.wg.Add(1)
	defer m.wg.Done()

	for {
		glog.V(2).Info("Getting device list")

		devs := m.getDeviceList()

		glog.V(2).Infof("Found %d device(s), sending to kubelet", len(devs))

		srv.Send(&pluginapi.ListAndWatchResponse{Devices: devs})

		select {
		case update, ok := <-m.healthUpd:
			if update && ok {
				continue
			}
			return fmt.Errorf("health updates ended")
		}
	}

	return nil
}

// Allocate is called during container creation so that the Device Plugin can run device specific
// operations and instruct Kubelet of the steps to make the Device available in the container
func (m *DevicePluginServer) Allocate(ctx context.Context, req *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	glog.V(2).Infof("%d container(s) requesting devices", len(req.ContainerRequests))
	resp := pluginapi.AllocateResponse{}

	for _, containerReq := range req.ContainerRequests {
		glog.V(2).Infof("Container is requesting %d device(s)", len(containerReq.DevicesIDs))

		for idx, id := range containerReq.DevicesIDs {
			dev, err := m.getDeviceById(id)
			if err != nil {
				return nil, err
			}

			var pathMaps []deviceMap
			path := dev.path

			glog.V(2).Infof("Adding %s to the container", dev.path)

			// Symlinks are handled specially for two reasons,
			//	1. 	usbfs doesn't play nice with symlinks so we need to recreate the host device path in
			//			the container. This is handled by altering the path used in the non symlink case.
			//	2.	because we can have several devices in a group on a host and we don't know which one
			//			was mapped into the container we use the group name as the device name and append a
			//			number of increasing order, starting at 0, so that we can use a consistent name in
			//			the container
			if fs.IsSymlink(path) {
				dir := filepath.Dir(path)

				symPath, err := fs.FollowSymlink(path)
				if err != nil {
					return nil, err
				}

				deviceName := fmt.Sprintf("%s%d", m.groupName, idx)

				glog.V(3).Infof("New symlink device name: %s", deviceName)

				newSymPath := filepath.Join(dir, deviceName)

				glog.V(3).Infof("New symlink device path: %s", newSymPath)

				if m.mapSymPaths {
					glog.V(3).Infof("Skipping symlink device path: %s", newSymPath)

					pathMaps = append(pathMaps, deviceMap{
						hostPath:      symPath,
						containerPath: newSymPath,
					})
				}

				path = symPath
			}

			pathMaps = append(pathMaps, deviceMap{
				hostPath:      path,
				containerPath: path,
			})

			containerResp := pluginapi.ContainerAllocateResponse{}

			for _, pathMap := range pathMaps {
				glog.V(2).Infof("host: %s, container: %s", pathMap.hostPath, pathMap.containerPath)

				containerResp.Devices = append(containerResp.Devices, &pluginapi.DeviceSpec{
					ContainerPath: pathMap.containerPath,
					HostPath:      pathMap.hostPath,
					Permissions:   "rw",
				})
			}

			resp.ContainerResponses = append(resp.ContainerResponses, &containerResp)
		}
	}

	return &resp, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase, before
// each container start. Device plugin can run device specific operations such as reseting the
// device before making devices available to the container
func (m *DevicePluginServer) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate from a list of available
// ones. The resulting preferred allocation is not guaranteed to be the allocation ultimately
// performed by the devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (m *DevicePluginServer) GetPreferredAllocation(context.Context, *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	return &pluginapi.PreferredAllocationResponse{}, nil
}
