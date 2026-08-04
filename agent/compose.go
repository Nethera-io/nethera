package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	publicHostPortStart = 20000
	publicHostPortEnd   = 45000
)

func generateComposeFile(composeContent, appName, deploymentID, applicationID, machineWireGuardIP string, previousPorts map[string]int, envFilePath string, managedFileMounts []managedFileMount, generatedEnv map[string]string) (string, map[string]int, []string, []publicEndpointReport, error) {
	return generateComposeFileWithReservedPorts(composeContent, appName, deploymentID, applicationID, machineWireGuardIP, previousPorts, nil, envFilePath, managedFileMounts, generatedEnv)
}

func generateComposeFileWithReservedPorts(composeContent, appName, deploymentID, applicationID, machineWireGuardIP string, previousPorts map[string]int, reservedHostPorts []int, envFilePath string, managedFileMounts []managedFileMount, generatedEnv map[string]string) (string, map[string]int, []string, []publicEndpointReport, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return "", nil, nil, nil, fmt.Errorf("compose yaml is invalid: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", nil, nil, nil, fmt.Errorf("compose yaml must be a mapping at the top level")
	}
	servicesNode := yamlMappingValue(root.Content[0], "services")
	if servicesNode == nil {
		output, err := marshalYAML(&root)
		return output, map[string]int{}, nil, nil, err
	}
	if servicesNode.Kind != yaml.MappingNode {
		return "", nil, nil, nil, fmt.Errorf("services must be a mapping")
	}
	allocatedPorts := map[string]int{}
	usedPorts := map[int]bool{}
	usedLANPorts := map[int]bool{}
	for _, port := range reservedHostPorts {
		if port > 0 && port <= 65535 {
			usedPorts[port] = true
		}
	}
	lanHost := firstLANAddress()
	if lanHost != "" {
		for _, port := range occupiedLocalTCPPorts(lanHost, publicHostPortStart, publicHostPortEnd) {
			usedLANPorts[port] = true
		}
	}
	oneShotServices := detectOneShotServices(servicesNode)
	expectedServices := make([]string, 0, len(servicesNode.Content)/2)
	publicEndpoints := []publicEndpointReport{}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceName := strings.TrimSpace(servicesNode.Content[index].Value)
		serviceNode := servicesNode.Content[index+1]
		if serviceName == "" || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		if !oneShotServices[serviceName] {
			expectedServices = append(expectedServices, serviceName)
		}
		if yamlMappingValue(serviceNode, "restart") == nil && !oneShotServices[serviceName] {
			yamlSetMappingValue(serviceNode, "restart", scalarStringNode("unless-stopped"))
		}
		mergeLabels(serviceNode, map[string]string{
			"nethera.managed":        "true",
			"nethera.deployment_id":  deploymentID,
			"nethera.application_id": applicationID,
		})
		if len(generatedEnv) > 0 {
			if strings.TrimSpace(envFilePath) == "" {
				return "", nil, nil, nil, fmt.Errorf("deployment env file path is required for generated env vars")
			}
			addEnvFile(serviceNode, envFilePath)
		}
		netheraNode := yamlMappingValue(serviceNode, "nethera")
		if netheraNode != nil && netheraNode.Kind == yaml.MappingNode && yamlMappingValue(netheraNode, "secrets") != nil {
			if strings.TrimSpace(envFilePath) == "" {
				return "", nil, nil, nil, fmt.Errorf("deployment secret env file path is required for service %s", serviceName)
			}
			addEnvFile(serviceNode, envFilePath)
			yamlRemoveNestedMappingKey(serviceNode, "nethera", "secrets")
		}
		if err := applyManagedFileMounts(serviceName, serviceNode, managedFileMounts); err != nil {
			return "", nil, nil, nil, err
		}
		if netheraNode != nil && netheraNode.Kind == yaml.MappingNode {
			yamlRemoveNestedMappingKey(serviceNode, "nethera", "files")
		}
		publicNode := (*yaml.Node)(nil)
		preferLAN := false
		if netheraNode != nil && netheraNode.Kind == yaml.MappingNode {
			publicNode = yamlMappingValue(netheraNode, "public")
			preferLAN = yamlBoolValue(yamlMappingValue(netheraNode, "preferLan"))
		}
		portsNode := yamlMappingValue(serviceNode, "ports")
		if portsNode != nil && portsNode.Kind != yaml.SequenceNode {
			return "", nil, nil, nil, fmt.Errorf("service %s ports must be a list", serviceName)
		}
		if publicNode != nil {
			if serviceUsesHostNetwork(serviceNode) {
				route, err := applyHostNetworkPublicEndpoint(serviceName, appName, deploymentID, publicNode, machineWireGuardIP, usedPorts, preferLAN, lanHost)
				if err != nil {
					return "", nil, nil, nil, err
				}
				allocatedPorts[fmt.Sprintf("%s:%d", route.ServiceName, route.TargetPort)] = route.HostPort
				publicEndpoints = append(publicEndpoints, route)
				yamlRemoveNestedMappingKey(serviceNode, "nethera", "public")
			} else {
				if portsNode == nil {
					portsNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
					yamlSetMappingValue(serviceNode, "ports", portsNode)
				}
				route, err := applyPublicEndpoint(serviceName, appName, deploymentID, serviceNode, publicNode, portsNode, machineWireGuardIP, previousPorts, usedPorts, usedLANPorts, preferLAN, lanHost)
				if err != nil {
					return "", nil, nil, nil, err
				}
				allocatedPorts[fmt.Sprintf("%s:%d", route.ServiceName, route.TargetPort)] = route.HostPort
				publicEndpoints = append(publicEndpoints, route)
				yamlRemoveNestedMappingKey(serviceNode, "nethera", "public")
			}
		}
		yamlRemoveMappingKey(serviceNode, "nethera")
		if portsNode == nil {
			continue
		}
		for _, portNode := range portsNode.Content {
			if portNode.Kind != yaml.ScalarNode {
				continue
			}
			nextValue, portKey, hostPort, changed, err := bindPortToWireGuard(portNode.Value, serviceName, machineWireGuardIP, previousPorts)
			if err != nil {
				return "", nil, nil, nil, err
			}
			if changed {
				portNode.Tag = "!!str"
				portNode.Value = nextValue
				allocatedPorts[portKey] = hostPort
			}
		}
	}
	output, err := marshalYAML(&root)
	if err != nil {
		return "", nil, nil, nil, err
	}
	return output, allocatedPorts, expectedServices, publicEndpoints, nil
}

func detectOneShotServices(servicesNode *yaml.Node) map[string]bool {
	oneShot := map[string]bool{}
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return oneShot
	}
	for index := 0; index+1 < len(servicesNode.Content); index += 2 {
		serviceNode := servicesNode.Content[index+1]
		if serviceNode == nil || serviceNode.Kind != yaml.MappingNode {
			continue
		}
		dependsOnNode := yamlMappingValue(serviceNode, "depends_on")
		if dependsOnNode == nil || dependsOnNode.Kind != yaml.MappingNode {
			continue
		}
		for depIndex := 0; depIndex+1 < len(dependsOnNode.Content); depIndex += 2 {
			dependencyName := strings.TrimSpace(dependsOnNode.Content[depIndex].Value)
			dependencyNode := dependsOnNode.Content[depIndex+1]
			if dependencyName == "" || dependencyNode == nil || dependencyNode.Kind != yaml.MappingNode {
				continue
			}
			conditionNode := yamlMappingValue(dependencyNode, "condition")
			if conditionNode != nil && strings.EqualFold(strings.TrimSpace(conditionNode.Value), "service_completed_successfully") {
				oneShot[dependencyName] = true
			}
		}
	}
	return oneShot
}

func extractComposeProjectName(composeContent string) string {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(composeContent), &root); err != nil {
		return ""
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return ""
	}
	nameNode := yamlMappingValue(root.Content[0], "name")
	if nameNode == nil || nameNode.Kind != yaml.ScalarNode {
		return ""
	}
	return strings.TrimSpace(nameNode.Value)
}

func serviceUsesHostNetwork(serviceNode *yaml.Node) bool {
	networkModeNode := yamlMappingValue(serviceNode, "network_mode")
	return networkModeNode != nil &&
		networkModeNode.Kind == yaml.ScalarNode &&
		strings.EqualFold(strings.TrimSpace(networkModeNode.Value), "host")
}

func applyHostNetworkPublicEndpoint(serviceName, appName, deploymentID string, publicNode *yaml.Node, machineWireGuardIP string, usedPorts map[int]bool, preferLAN bool, lanHost string) (publicEndpointReport, error) {
	if machineWireGuardIP == "" {
		return publicEndpointReport{}, fmt.Errorf("machine wireguard IP is required to bind public endpoint for service %s", serviceName)
	}
	targetPort, err := publicTargetPort(publicNode)
	if err != nil {
		return publicEndpointReport{}, fmt.Errorf("service %s %w", serviceName, err)
	}
	if usedPorts[targetPort] {
		return publicEndpointReport{}, fmt.Errorf("host network public port %d is already used by another public endpoint", targetPort)
	}
	usedPorts[targetPort] = true
	route := publicEndpointReport{
		ServiceName: serviceName,
		Subdomain:   buildSubdomain(appName, serviceName, deploymentID),
		HostPort:    targetPort,
		TargetPort:  targetPort,
	}
	if preferLAN {
		if lanHost == "" {
			return publicEndpointReport{}, fmt.Errorf("service %s nethera.preferLan requires a detected LAN address", serviceName)
		}
		route.PreferLAN = true
		route.LANHost = lanHost
		route.LANPort = targetPort
	}
	return route, nil
}

type parsedPortSpec struct {
	TargetPort int
	HostPort   int
	Bare       bool
	Protocol   string
}

func applyPublicEndpoint(serviceName, appName, deploymentID string, serviceNode, publicNode, portsNode *yaml.Node, machineWireGuardIP string, previousPorts map[string]int, usedPorts map[int]bool, usedLANPorts map[int]bool, preferLAN bool, lanHost string) (publicEndpointReport, error) {
	if machineWireGuardIP == "" {
		return publicEndpointReport{}, fmt.Errorf("machine wireguard IP is required to bind public endpoint for service %s", serviceName)
	}
	parsedPorts := []parsedPortSpec{}
	for _, portNode := range portsNode.Content {
		if portNode.Kind != yaml.ScalarNode {
			continue
		}
		parsed, err := parseComposePortSpec(portNode.Value)
		if err != nil {
			return publicEndpointReport{}, fmt.Errorf("invalid port mapping for service %s: %w", serviceName, err)
		}
		parsedPorts = append(parsedPorts, parsed)
		if parsed.HostPort > 0 {
			usedPorts[parsed.HostPort] = true
		}
	}
	targetPort, err := publicTargetPort(publicNode)
	if err != nil {
		return publicEndpointReport{}, fmt.Errorf("service %s %w", serviceName, err)
	}
	var matching parsedPortSpec
	for _, parsed := range parsedPorts {
		if parsed.TargetPort == targetPort {
			matching = parsed
			break
		}
	}
	portKey := fmt.Sprintf("%s:%d", serviceName, targetPort)
	hostPort := previousPorts[portKey]
	if hostPort <= 0 || usedPorts[hostPort] {
		hostPort = allocatePublicPort(usedPorts)
	}
	if hostPort <= 0 {
		return publicEndpointReport{}, fmt.Errorf("unable to allocate a public host port for service %s", serviceName)
	}
	lanPort := 0
	if preferLAN {
		if lanHost == "" {
			return publicEndpointReport{}, fmt.Errorf("service %s nethera.preferLan requires a detected LAN address", serviceName)
		}
		lanPort = targetPort
		if usedLANPorts[lanPort] {
			return publicEndpointReport{}, fmt.Errorf("service %s nethera.preferLan cannot bind LAN port %d because another service in this deployment already uses it", serviceName, lanPort)
		}
		usedLANPorts[lanPort] = true
	}
	usedPorts[hostPort] = true
	filtered := make([]*yaml.Node, 0, len(portsNode.Content)+1)
	for _, portNode := range portsNode.Content {
		if portNode.Kind != yaml.ScalarNode {
			filtered = append(filtered, portNode)
			continue
		}
		parsed, err := parseComposePortSpec(portNode.Value)
		if err == nil && parsed.TargetPort == targetPort {
			continue
		}
		filtered = append(filtered, portNode)
	}
	filtered = append(filtered, scalarStringNode(fmt.Sprintf("%s:%d:%d%s", machineWireGuardIP, hostPort, targetPort, matching.Protocol)))
	if preferLAN {
		filtered = append(filtered, scalarStringNode(fmt.Sprintf("%s:%d:%d%s", lanHost, lanPort, targetPort, matching.Protocol)))
	}
	portsNode.Content = filtered
	route := publicEndpointReport{
		ServiceName: serviceName,
		Subdomain:   buildSubdomain(appName, serviceName, deploymentID),
		HostPort:    hostPort,
		TargetPort:  targetPort,
	}
	if preferLAN {
		route.PreferLAN = true
		route.LANHost = lanHost
		route.LANPort = lanPort
	}
	return route, nil
}

func publicTargetPort(publicNode *yaml.Node) (int, error) {
	if publicNode.Kind == yaml.SequenceNode {
		if len(publicNode.Content) == 0 {
			return 0, fmt.Errorf("nethera.public must include at least one port")
		}
		if len(publicNode.Content) > 1 {
			return 0, fmt.Errorf("nethera.public currently supports one public port")
		}
		return parsePublicPort(publicNode.Content[0], "nethera.public")
	}
	return parsePublicPort(publicNode, "nethera.public")
}

func parsePublicPort(node *yaml.Node, fieldName string) (int, error) {
	if node == nil || node.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("%s must be a valid container port", fieldName)
	}
	port, err := strconv.Atoi(strings.TrimSpace(node.Value))
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("%s must be a valid container port", fieldName)
	}
	return port, nil
}

func yamlBoolValue(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(node.Value), "true")
}

func firstLANAddress() string {
	for _, address := range collectLANAddresses() {
		trimmed := strings.TrimSpace(address)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseComposePortSpec(value string) (parsedPortSpec, error) {
	trimmed := strings.TrimSpace(value)
	protocol := ""
	body := trimmed
	if before, after, ok := strings.Cut(trimmed, "/"); ok {
		body = before
		protocol = "/" + after
	}
	parts := strings.Split(body, ":")
	if len(parts) == 0 {
		return parsedPortSpec{}, fmt.Errorf("empty port mapping")
	}
	targetPort, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
	if err != nil || targetPort <= 0 || targetPort > 65535 {
		return parsedPortSpec{}, fmt.Errorf("invalid target port")
	}
	hostPort := 0
	if len(parts) >= 2 {
		hostPort, _ = strconv.Atoi(strings.TrimSpace(parts[len(parts)-2]))
	}
	return parsedPortSpec{TargetPort: targetPort, HostPort: hostPort, Bare: len(parts) == 1, Protocol: protocol}, nil
}

func allocatePublicPort(usedPorts map[int]bool) int {
	for port := publicHostPortStart; port <= publicHostPortEnd; port++ {
		if !usedPorts[port] {
			return port
		}
	}
	return 0
}

func sanitizeSubdomainLabel(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, ch := range lower {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			builder.WriteRune(ch)
		} else {
			builder.WriteRune('-')
		}
	}
	out := strings.Trim(builder.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

func buildSubdomain(appName, serviceName, deploymentID string) string {
	return fmt.Sprintf("%s-%s-%s", sanitizeSubdomainLabel(appName), sanitizeSubdomainLabel(serviceName), sanitizeSubdomainLabel(deploymentID))
}

func bindPortToWireGuard(value, serviceName, machineWireGuardIP string, _ map[string]int) (string, string, int, bool, error) {
	const netheraPortBindMarkerIP = "127.255.0.1"
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, "", 0, false, nil
	}
	body := trimmed
	protocol := ""
	if before, after, ok := strings.Cut(trimmed, "/"); ok {
		body = before
		protocol = "/" + after
	}
	parts := strings.Split(body, ":")
	if len(parts) != 3 || strings.TrimSpace(parts[0]) != netheraPortBindMarkerIP {
		return value, "", 0, false, nil
	}
	targetPort, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil || targetPort <= 0 || targetPort > 65535 {
		return "", "", 0, false, fmt.Errorf("invalid port mapping for service %s: %s", serviceName, value)
	}
	hostPort, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil || hostPort <= 0 || hostPort > 65535 {
		return "", "", 0, false, fmt.Errorf("invalid host port mapping for service %s: %s", serviceName, value)
	}
	portKey := fmt.Sprintf("%s:%d", serviceName, targetPort)
	if machineWireGuardIP == "" {
		return "", "", 0, false, fmt.Errorf("machine wireguard IP is required to bind public endpoint %s", portKey)
	}
	return fmt.Sprintf("%s:%d:%d%s", machineWireGuardIP, hostPort, targetPort, protocol), portKey, hostPort, true, nil
}

func mergeLabels(serviceNode *yaml.Node, labels map[string]string) {
	labelsNode := yamlMappingValue(serviceNode, "labels")
	if labelsNode == nil {
		next := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := sortedLabelKeys(labels)
		for _, key := range keys {
			next.Content = append(next.Content, scalarStringNode(key), scalarStringNode(labels[key]))
		}
		yamlSetMappingValue(serviceNode, "labels", next)
		return
	}
	switch labelsNode.Kind {
	case yaml.MappingNode:
		keys := sortedLabelKeys(labels)
		for _, key := range keys {
			yamlSetMappingValue(labelsNode, key, scalarStringNode(labels[key]))
		}
	case yaml.SequenceNode:
		existing := map[string]int{}
		for index, item := range labelsNode.Content {
			if item.Kind != yaml.ScalarNode {
				continue
			}
			key, _, ok := strings.Cut(item.Value, "=")
			if ok {
				existing[key] = index
			}
		}
		keys := sortedLabelKeys(labels)
		for _, key := range keys {
			value := key + "=" + labels[key]
			if index, ok := existing[key]; ok {
				labelsNode.Content[index].Value = value
			} else {
				labelsNode.Content = append(labelsNode.Content, scalarStringNode(value))
			}
		}
	}
}

func addEnvFile(serviceNode *yaml.Node, envFilePath string) {
	envFileNode := yamlMappingValue(serviceNode, "env_file")
	if envFileNode == nil {
		yamlSetMappingValue(serviceNode, "env_file", &yaml.Node{
			Kind: yaml.SequenceNode,
			Tag:  "!!seq",
			Content: []*yaml.Node{
				scalarStringNode(envFilePath),
			},
		})
		return
	}
	switch envFileNode.Kind {
	case yaml.SequenceNode:
		for _, item := range envFileNode.Content {
			if item.Kind == yaml.ScalarNode && item.Value == envFilePath {
				return
			}
		}
		envFileNode.Content = append(envFileNode.Content, scalarStringNode(envFilePath))
	case yaml.ScalarNode:
		if envFileNode.Value == envFilePath {
			return
		}
		envFileNode.Kind = yaml.SequenceNode
		envFileNode.Tag = "!!seq"
		envFileNode.Content = []*yaml.Node{
			scalarStringNode(envFileNode.Value),
			scalarStringNode(envFilePath),
		}
		envFileNode.Value = ""
	default:
		yamlSetMappingValue(serviceNode, "env_file", &yaml.Node{
			Kind: yaml.SequenceNode,
			Tag:  "!!seq",
			Content: []*yaml.Node{
				scalarStringNode(envFilePath),
			},
		})
	}
}

func applyManagedFileMounts(serviceName string, serviceNode *yaml.Node, mounts []managedFileMount) error {
	serviceMounts := []managedFileMount{}
	for _, mount := range mounts {
		if mount.ServiceName == serviceName {
			serviceMounts = append(serviceMounts, mount)
		}
	}
	if len(serviceMounts) == 0 {
		return nil
	}

	targets := map[string]bool{}
	for _, target := range existingVolumeTargets(serviceNode) {
		targets[target] = true
	}
	for _, mount := range serviceMounts {
		if targets[mount.Target] {
			return fmt.Errorf("Managed file target %s conflicts with an existing volume mount for service %s.", mount.Target, serviceName)
		}
		targets[mount.Target] = true
	}

	volumesNode := yamlMappingValue(serviceNode, "volumes")
	if volumesNode == nil {
		volumesNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		yamlSetMappingValue(serviceNode, "volumes", volumesNode)
	}
	if volumesNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("service %s volumes must be a list", serviceName)
	}
	for _, mount := range serviceMounts {
		volumesNode.Content = append(volumesNode.Content, scalarStringNode(fmt.Sprintf("%s:%s:ro", mount.HostPath, mount.Target)))
	}
	return nil
}

func existingVolumeTargets(serviceNode *yaml.Node) []string {
	volumesNode := yamlMappingValue(serviceNode, "volumes")
	if volumesNode == nil || volumesNode.Kind != yaml.SequenceNode {
		return nil
	}
	targets := []string{}
	for _, item := range volumesNode.Content {
		target := volumeTarget(item)
		if target != "" {
			targets = append(targets, target)
		}
	}
	return targets
}

func volumeTarget(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if node.Kind == yaml.MappingNode {
		for _, key := range []string{"target", "dst", "destination"} {
			value := yamlMappingValue(node, key)
			if value != nil && value.Kind == yaml.ScalarNode {
				return strings.TrimSpace(value.Value)
			}
		}
		return ""
	}
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ":")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func sortedLabelKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func yamlSetMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			node.Content[index+1] = value
			return
		}
	}
	node.Content = append(node.Content, scalarStringNode(key), value)
}

func yamlRemoveMappingKey(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	filtered := make([]*yaml.Node, 0, len(node.Content))
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			continue
		}
		filtered = append(filtered, node.Content[index], node.Content[index+1])
	}
	node.Content = filtered
}

func yamlRemoveNestedMappingKey(node *yaml.Node, parentKey string, childKey string) {
	parent := yamlMappingValue(node, parentKey)
	if parent == nil || parent.Kind != yaml.MappingNode {
		return
	}
	yamlRemoveMappingKey(parent, childKey)
	if len(parent.Content) == 0 {
		yamlRemoveMappingKey(node, parentKey)
	}
}

func scalarStringNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func marshalYAML(node *yaml.Node) (string, error) {
	var builder strings.Builder
	encoder := yaml.NewEncoder(&builder)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		_ = encoder.Close()
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return builder.String(), nil
}
