// Package viamonvif provides ONVIF integration to the viamrtsp module
package viamonvif

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/viam-modules/viamrtsp/viamonvif/device"
	"github.com/viam-modules/viamrtsp/viamonvif/xsd/onvif"

	"go.viam.com/rdk/logging"
)

// DiscoverCameras performs WS-Discovery
// then uses ONVIF queries to get available RTSP addresses and supplementary info.
func DiscoverCameras(creds []device.Credentials, manualXAddrs []*url.URL, logger logging.Logger) (*CameraInfoList, error) {
	var discoveredCameras []CameraInfo
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve network interfaces: %w", err)
	}

	discovered := map[string]*url.URL{}
	for _, xaddr := range manualXAddrs {
		discovered[xaddr.Host] = xaddr
	}
	for _, iface := range ifaces {
		xaddrs, err := WSDiscovery(ctx, logger, iface)
		if err != nil {
			logger.Debugf("WS-Discovery skipping interface %s: due to error from SendProbe: %w", iface.Name, err)
			continue
		}
		for _, xaddr := range xaddrs {
			discovered[xaddr.Host] = xaddr
		}
	}

	for _, xaddr := range discovered {
		logger.Debugf("Connecting to ONVIF device with URL: %s", xaddr.Host)
		cameraInfo, err := DiscoverCamerasOnXAddr(ctx, xaddr, creds, logger)
		if err != nil {
			logger.Warnf("failed to connect to ONVIF device %v", err)
			continue
		}
		discoveredCameras = append(discoveredCameras, cameraInfo)
	}
	return &CameraInfoList{Cameras: discoveredCameras}, nil
}

// OnvifDevice is an interface to abstract device methods used in the code.
// Used instead of onvif.Device to allow for mocking in tests.
type OnvifDevice interface {
	GetDeviceInformation() (device.DeviceInfo, error)
	GetProfiles() (device.GetProfilesResponse, error)
	GetStreamUri(profile onvif.Profile, creds device.Credentials) (*url.URL, error)
}

// CameraInfo holds both the RTSP URLs and supplementary camera details.
type CameraInfo struct {
	WSDiscoveryXAddr string   `json:"ws_discovery_x_addr"`
	RTSPURLs         []string `json:"rtsp_urls"`
	Manufacturer     string   `json:"manufacturer"`
	Model            string   `json:"model"`
	SerialNumber     string   `json:"serial_number"`
	FirmwareVersion  string   `json:"firmware_version"`
	HardwareId       string   `json:"hardware_id"`
}

// CameraInfoList is a struct containing a list of CameraInfo structs.
type CameraInfoList struct {
	Cameras []CameraInfo `json:"cameras"`
}

func DiscoverCamerasOnXAddr(
	ctx context.Context,
	xaddr *url.URL,
	creds []device.Credentials,
	logger logging.Logger,
) (CameraInfo, error) {
	var zero CameraInfo
	for _, cred := range creds {
		if ctx.Err() != nil {
			return zero, fmt.Errorf("context canceled while connecting to ONVIF device: %s", xaddr)
		}
		// This calls GetCapabilities
		dev, err := device.NewDevice(device.DeviceParams{
			Xaddr:    xaddr,
			Username: cred.User,
			Password: cred.Pass,
		}, logger)
		if err != nil {
			logger.Debugf("Failed to get camera capabilities from %s: %v", xaddr, err)
			continue
		}

		logger.Debugf("ip: %s GetCapabilities: DeviceInfo: %#v, Endpoints: %#v", xaddr, dev)
		cameraInfo, err := GetCameraInfo(dev, xaddr, cred, logger)
		if err != nil {
			logger.Warnf("Failed to get camera info from %s: %v", xaddr, err)
			continue
		}
		// once we have addeed a camera info break
		return cameraInfo, nil
	}
	return zero, fmt.Errorf("no credentials matched IP %s", xaddr)
}

// extractXAddrsFromProbeMatch extracts XAddrs from the WS-Discovery ProbeMatch response.
func extractXAddrsFromProbeMatch(response string, logger logging.Logger) []*url.URL {
	type ProbeMatch struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			ProbeMatches struct {
				ProbeMatch []struct {
					XAddrs string `xml:"XAddrs"`
				} `xml:"ProbeMatch"`
			} `xml:"ProbeMatches"`
		} `xml:"Body"`
	}

	var probeMatch ProbeMatch
	err := xml.NewDecoder(strings.NewReader(response)).Decode(&probeMatch)
	if err != nil {
		logger.Warnf("error unmarshalling ONVIF discovery xml response: %w\nFull xml resp: %s", err, response)
	}

	xaddrs := []*url.URL{}
	for _, match := range probeMatch.Body.ProbeMatches.ProbeMatch {
		for _, xaddr := range strings.Split(match.XAddrs, " ") {
			parsedURL, err := url.Parse(xaddr)
			if err != nil {
				logger.Warnf("failed to parse XAddr %s: %w", xaddr, err)
				continue
			}

			xaddrs = append(xaddrs, parsedURL)
		}
	}

	return xaddrs
}

// GetCameraInfo uses the ONVIF Media service to get the RTSP stream URLs and camera details.
func GetCameraInfo(dev OnvifDevice, xaddr *url.URL, creds device.Credentials, logger logging.Logger) (CameraInfo, error) {
	var zero CameraInfo
	// Fetch device information (manufacturer, serial number, etc.)
	deviceInfo, err := dev.GetDeviceInformation()
	if err != nil {
		return zero, fmt.Errorf("failed to read device information response body: %w", err)
	}

	// Call the ONVIF Media service to get the available media profiles using the same device instance
	rtspURLs, err := GetRTSPStreamURIsFromProfiles(dev, creds, logger)
	if err != nil {
		return zero, fmt.Errorf("failed to get RTSP URLs: %w", err)
	}

	cameraInfo := CameraInfo{
		WSDiscoveryXAddr: xaddr.Host,
		RTSPURLs:         rtspURLs,
		Manufacturer:     deviceInfo.Body.GetDeviceInformationResponse.Manufacturer,
		Model:            deviceInfo.Body.GetDeviceInformationResponse.Model,
		SerialNumber:     deviceInfo.Body.GetDeviceInformationResponse.SerialNumber,
		FirmwareVersion:  deviceInfo.Body.GetDeviceInformationResponse.FirmwareVersion,
		HardwareId:       deviceInfo.Body.GetDeviceInformationResponse.HardwareId,
	}

	return cameraInfo, nil
}

// GetRTSPStreamURIsFromProfiles uses the ONVIF Media service to get the RTSP stream URLs for all available profiles.
func GetRTSPStreamURIsFromProfiles(dev OnvifDevice, creds device.Credentials, logger logging.Logger) ([]string, error) {
	resp, err := dev.GetProfiles()
	if err != nil {
		return nil, err
	}

	// Resultant slice of RTSP URIs
	var rtspUris []string
	// Iterate over all profiles and get the RTSP stream URI for each one
	for _, profile := range resp.Body.GetProfilesResponse.Profiles {
		uri, err := dev.GetStreamUri(profile, creds)
		if err != nil {
			logger.Warnf(err.Error())
			continue
		}

		rtspUris = append(rtspUris, uri.String())
	}

	return rtspUris, nil
}
