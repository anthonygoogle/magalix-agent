package gateway

import (
	"github.com/MagalixCorp/magalix-agent/v2/agent"
	"github.com/MagalixCorp/magalix-agent/v2/client"
	"github.com/MagalixCorp/magalix-agent/v2/proto"
	"github.com/MagalixCorp/magalix-agent/v2/utils"
	"math"
	"time"
)

const metricsBatchMaxSize = 1000

func (g *MagalixGateway) SendMetrics(metrics []*agent.Metric) error {
	noOfBatches := int(math.Ceil(float64(len(metrics))/float64(metricsBatchMaxSize)))
	lastBatchSize := len(metrics) % metricsBatchMaxSize
	for i := 0; i < noOfBatches; i++ {
		start := i * metricsBatchMaxSize
		var end int
		if i == noOfBatches- 1 && lastBatchSize > 0 {
			end = start + lastBatchSize
		} else {
			end = start + metricsBatchMaxSize
		}
		g.sendMetricsBatch(g.gwClient, metrics[start:end])
	}
	return nil
}

// SendMetrics bulk send metrics
func (g *MagalixGateway) sendMetricsBatch(c *client.Client, metrics []*agent.Metric) {
	var packet interface{}
	var packetKind proto.PacketKind

	var req proto.PacketMetricsStoreV2Request
	for _, metric := range metrics {
		req = append(req, proto.MetricStoreV2Request{
			Name:           metric.Name,
			Type:           metric.Type,
			NodeName:       metric.NodeName,
			NodeIP:         metric.NodeIP,
			NamespaceName:  metric.NamespaceName,
			ControllerName: metric.ControllerName,
			ControllerKind: metric.ControllerKind,
			ContainerName:  metric.ContainerName,
			Timestamp:      metric.Timestamp,
			Value:          metric.Value,
			PodName:        metric.PodName,
			AdditionalTags: metric.AdditionalTags,
		})

	}
	packet = req
	packetKind = proto.PacketKindMetricsStoreV2Request

	c.Pipe(client.Package{
		Kind:        packetKind,
		ExpiryTime:  utils.After(2 * time.Hour),
		ExpiryCount: 100,
		Priority:    4,
		Retries:     10,
		Data:        packet,
	})
}
