package proxy

import (
	"sync"
	"time"

	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/admin"
	"github.com/mailgun/kafka-pixy/config"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/consumer/consumerimpl"
	"github.com/mailgun/kafka-pixy/offsetmgr"
	"github.com/mailgun/kafka-pixy/producer"
	"github.com/mailgun/sarama"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	initEventsChMapCapacity = 256
)

var (
	ErrUnavailable = errors.New("service is shutting down")

	noAck   = Ack{partition: -1}
	autoAck = Ack{partition: -2}
)

// T implements a proxy to a particular Kafka/ZooKeeper cluster.
type T struct {
	actDesc    *actor.Descriptor
	cfg        *config.Proxy
	kafkaClt   sarama.Client
	offsetMgrF offsetmgr.Factory

	adminMu sync.RWMutex
	admin   *admin.T

	producerMu sync.RWMutex
	producer   *producer.T

	consumerMu sync.RWMutex
	consumer   consumer.T

	// FIXME: We never remove stale elements from eventsChMap. It is sort of ok
	// FIXME: since the number of group/topic/partition combinations is fairly
	// FIXME: limited and should not cause any significant system memory usage.
	eventsChMapMu sync.RWMutex
	eventsChMap   map[eventsChID]chan<- consumer.Event
}

type Ack struct {
	partition int32
	offset    int64
}

// NewAck creates an acknowledgement instance from a partition and an offset.
// Note that group and topic are not included. Respective values that are
// passed to proxy.Consume function along with the ack are gonna be used.
func NewAck(partition int32, offset int64) (Ack, error) {
	if partition < 0 {
		return Ack{}, errors.Errorf("bad partition: %d", partition)
	}
	if offset < 0 {
		return Ack{}, errors.Errorf("bad offset: %d", offset)
	}
	return Ack{partition, offset}, nil
}

// NoAck returns an ack value that should be passed to proxy.Consume function
// when a caller does not want to acknowledge anything.
func NoAck() Ack {
	return noAck
}

// AutoAck returns an ack value that should be passed to proxy.Consume function
// when a caller wants the consumed message to be acknowledged immediately.
func AutoAck() Ack {
	return autoAck
}

type eventsChID struct {
	group     string
	topic     string
	partition int32
}

// Spawn creates a proxy instance and starts its internal goroutines.
func Spawn(parentActDesc *actor.Descriptor, name string, cfg *config.Proxy) (*T, error) {
	p := T{
		actDesc:     parentActDesc.NewChild(name),
		cfg:         cfg,
		eventsChMap: make(map[eventsChID]chan<- consumer.Event, initEventsChMapCapacity),
	}
	var err error

	if p.kafkaClt, err = sarama.NewClient(cfg.Kafka.SeedPeers, cfg.SaramaClientCfg()); err != nil {
		return nil, errors.Wrap(err, "failed to create Kafka client")
	}
	p.offsetMgrF = offsetmgr.SpawnFactory(p.actDesc, cfg, p.kafkaClt)
	if p.producer, err = producer.Spawn(p.actDesc, cfg); err != nil {
		return nil, errors.Wrap(err, "failed to spawn producer")
	}
	if p.consumer, err = consumerimpl.Spawn(p.actDesc, cfg, p.offsetMgrF); err != nil {
		return nil, errors.Wrap(err, "failed to spawn consumer")
	}
	if p.admin, err = admin.Spawn(p.actDesc, cfg); err != nil {
		return nil, errors.Wrap(err, "failed to spawn admin")
	}
	return &p, nil
}

// Stop terminates the proxy instances synchronously.
func (p *T) Stop() {
	var wg sync.WaitGroup

	p.producerMu.RLock()
	if p.producer != nil {
		actor.Spawn(p.actDesc.NewChild("prod_stop"), &wg, p.stopProducer)
	}
	p.producerMu.RUnlock()

	p.consumerMu.RLock()
	if p.consumer != nil {
		actor.Spawn(p.actDesc.NewChild("cons_stop"), &wg, p.stopConsumer)
	}
	p.consumerMu.RUnlock()

	p.adminMu.RLock()
	if p.admin != nil {
		actor.Spawn(p.actDesc.NewChild("adm_stop"), &wg, p.stopAdmin)
	}
	p.adminMu.RUnlock()

	wg.Wait()
	if p.offsetMgrF != nil {
		p.offsetMgrF.Stop()
	}
	if p.kafkaClt != nil {
		p.kafkaClt.Close()
	}
}

func (p *T) stopConsumer() {
	p.consumerMu.Lock()
	cons := p.consumer
	p.consumer = nil
	p.consumerMu.Unlock()
	cons.Stop()
}

func (p *T) stopProducer() {
	p.producerMu.Lock()
	prod := p.producer
	p.producer = nil
	p.producerMu.Unlock()
	prod.Stop()
}

func (p *T) stopAdmin() {
	p.adminMu.Lock()
	p.admin.Stop()
	p.adminMu.Unlock()
}

// Produce submits a message to the specified `topic` of the Kafka cluster
// using `key` to identify a destination partition. The exact algorithm used to
// map keys to partitions is implementation specific but it is guaranteed that
// it returns consistent results. If `key` is `nil`, then the message is placed
// into a random partition.
//
// Errors usually indicate a catastrophic failure of the Kafka cluster, or
// missing topic if there cluster is not configured to auto create topics.
func (p *T) Produce(topic string, key, message sarama.Encoder) (*sarama.ProducerMessage, error) {
	p.producerMu.RLock()
	if p.producer == nil {
		p.producerMu.RUnlock()
		return nil, ErrUnavailable
	}
	responseCh := p.producer.AsyncProduce(topic, key, message)
	p.producerMu.RUnlock()

	rs := <-responseCh
	return rs.Msg, rs.Err
}

// AsyncProduce is an asynchronously counterpart of the `Produce` function.
// Errors are silently ignored.
func (p *T) AsyncProduce(topic string, key, message sarama.Encoder) {
	p.producerMu.RLock()
	if p.producer == nil {
		p.producerMu.RUnlock()
		return
	}
	p.producer.AsyncProduce(topic, key, message)
	p.producerMu.RUnlock()
}

// Consume consumes a message from the specified topic on behalf of the
// specified consumer group. If there are no more new messages in the topic
// at the time of the request then it will block for
// `Config.Consumer.LongPollingTimeout`. If no new message is produced during
// that time, then `ErrRequestTimeout` is returned.
//
// Note that during state transitions topic subscribe<->unsubscribe and
// consumer group register<->deregister the method may return either
// `ErrBufferOverflow` or `ErrRequestTimeout` even when there are messages
// available for consumption. In that case the user should back off a bit
// and then repeat the request.
func (p *T) Consume(group, topic string, ack Ack) (consumer.Message, error) {
	if ack != noAck && ack != autoAck {
		p.eventsChMapMu.RLock()
		eventsChID := eventsChID{group, topic, ack.partition}
		eventsCh, ok := p.eventsChMap[eventsChID]
		p.eventsChMapMu.RUnlock()
		if ok {
			go func() {
				select {
				case eventsCh <- consumer.Ack(ack.offset):
				case <-time.After(p.cfg.Consumer.LongPollingTimeout):
					p.actDesc.Log().WithFields(log.Fields{
						"kafka.group":     group,
						"kafka.topic":     topic,
						"kafka.partition": ack.partition,
					}).Errorf("ack timeout: offset=%d", ack.offset)
				}
			}()
		}
	}

	p.consumerMu.RLock()
	if p.consumer == nil {
		p.consumerMu.RUnlock()
		return consumer.Message{}, ErrUnavailable
	}
	responseCh := p.consumer.AsyncConsume(group, topic)
	p.consumerMu.RUnlock()

	rs := <-responseCh
	if rs.Err != nil {
		return consumer.Message{}, rs.Err
	}

	eventsChID := eventsChID{group, topic, rs.Msg.Partition}
	p.eventsChMapMu.Lock()
	p.eventsChMap[eventsChID] = rs.Msg.EventsCh
	p.eventsChMapMu.Unlock()

	if ack == autoAck {
		rs.Msg.EventsCh <- consumer.Ack(rs.Msg.Offset)
	}
	return rs.Msg, nil
}

func (p *T) Ack(group, topic string, ack Ack) error {
	eventsChID := eventsChID{group, topic, ack.partition}
	p.eventsChMapMu.RLock()
	eventsCh, ok := p.eventsChMap[eventsChID]
	p.eventsChMapMu.RUnlock()
	if !ok {
		return errors.Errorf("acks channel missing for %v", eventsChID)
	}
	select {
	case eventsCh <- consumer.Ack(ack.offset):
	case <-time.After(p.cfg.Consumer.LongPollingTimeout):
		return errors.New("ack timeout")
	}
	return nil
}

// GetGroupOffsets for every partition of the specified topic it returns the
// current offset range along with the latest offset and metadata committed by
// the specified consumer group.
func (p *T) GetGroupOffsets(group, topic string) ([]admin.PartitionOffset, error) {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return nil, ErrUnavailable
	}
	return p.admin.GetGroupOffsets(group, topic)
}

// SetGroupOffsets commits specific offset values along with metadata for a list
// of partitions of a particular topic on behalf of the specified group.
func (p *T) SetGroupOffsets(group, topic string, offsets []admin.PartitionOffset) error {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return ErrUnavailable
	}
	return p.admin.SetGroupOffsets(group, topic, offsets)
}

// GetTopicConsumers returns client-id -> consumed-partitions-list mapping
// for a clients from a particular consumer group and a particular topic.
func (p *T) GetTopicConsumers(group, topic string) (map[string][]int32, error) {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return nil, ErrUnavailable
	}
	return p.admin.GetTopicConsumers(group, topic)
}

// GetAllTopicConsumers returns group -> client-id -> consumed-partitions-list
// mapping for a particular topic. Warning, the function performs scan of all
// consumer groups registered in ZooKeeper and therefore can take a lot of time.
func (p *T) GetAllTopicConsumers(topic string) (map[string]map[string][]int32, error) {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return nil, ErrUnavailable
	}
	return p.admin.GetAllTopicConsumers(topic)
}

// ListTopics returns a list of all topics existing in the Kafka cluster.
func (p *T) ListTopics(withPartitions, withConfig bool) ([]admin.TopicMetadata, error) {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return nil, ErrUnavailable
	}
	return p.admin.ListTopics(withPartitions, withConfig)
}

// GetTopicMetadata returns a topic metadata. An optional partition metadata
// can be requested and/or detailed topic configuration can be requested.
func (p *T) GetTopicMetadata(topic string, withPartitions, withConfig bool) (admin.TopicMetadata, error) {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	if p.admin == nil {
		return admin.TopicMetadata{}, ErrUnavailable
	}
	return p.admin.GetTopicMetadata(topic, withPartitions, withConfig)
}
