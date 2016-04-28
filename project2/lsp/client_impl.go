// Contains the implementation of a LSP client.

package lsp

import "errors"
import "encoding/json"
import "fmt"
import "os"
import "time"
import "github.com/minhtrangvy/distributed_bitcoin_miner/project2/lspnet"
// import "reflect"

const (
	MaxUint = ^uint(0)
	MaxInt = int(MaxUint >> 1)
)

type client struct {
	connID 			int
	connection		*lspnet.UDPConn
	address			*lspnet.UDPAddr
	currWriteSN		int
	lowestUnackSN 	int
	expectedSN	 	int						// SN we expect to receive next

	connectCh		(chan *Message)
	readCh			(chan *Message) 		// data messages to be printed
	writeCh			(chan *Message) 		// data messages to be written to server

	closeCh			(chan int)				// Close() has been called
	isClosed		bool

	intermedReadCh  (chan *Message)
	epochCh			(<-chan time.Time)
	dataWindow		map[int]*Message		// map from SN to *Message of unacknowledged data messages we have sent
	ackWindow		map[int]*Message		// map of the last windowSize acks that we have sent

	numEpochs		int 					// number of epochs that have occurred
	windowSize		int
	epochMilli		int
	epochLimit		int
	verbose			bool
}

// NewClient creates, initiates, and returns a new client. This function
// should return after a connection with the server has been established
// (i.e., the client has received an Ack message from the server in response
// to its connection request), and should return a non-nil error if a
// connection could not be made (i.e., if after K epochs, the client still
// hasn't received an Ack message from the server in response to its K
// connection requests).
//
// hostport is a colon-separated string identifying the server's host address
// and port number (i.e., "localhost:9999").
func NewClient(hostport string, params *Params) (Client, error) {

	current_client := &client {
		connID:			0,
		currWriteSN: 	1,
		lowestUnackSN: 	0,
		expectedSN: 	1,							// SN we expect to receive next

		connectCh:		make(chan *Message, CHANNEL_SIZE),
		readCh:			make(chan *Message, CHANNEL_SIZE), 		// data messages to be printed
		writeCh:		make(chan *Message, CHANNEL_SIZE), 		// data messages to be written to server

		closeCh:		make(chan int),				// Close() has been called
		isClosed:		false,

		intermedReadCh: make(chan *Message, CHANNEL_SIZE),
		epochCh:		make(<-chan time.Time),
		dataWindow:		make(map[int]*Message),		// map from SN to *Message of unacknowledged data messages we have sent
		ackWindow:		make(map[int]*Message),		// map of the last windowSize acks that we have sent

		numEpochs:		0, 					// number of epochs that have occurred
		windowSize: 	params.WindowSize,
		epochMilli: 	params.EpochMillis,
		epochLimit: 	params.EpochLimit,
		verbose: 		true,
	}

	serverAddr, resolve_err := lspnet.ResolveUDPAddr("udp", hostport)
	current_client.PrintError(resolve_err)

	// Send connect message to server
	current_conn, dial_err := lspnet.DialUDP("udp", nil, serverAddr)
	current_client.PrintError(dial_err)

	current_client.connection = current_conn

	// Go routines
	go current_client.master()
	go current_client.read()
	go current_client.epoch()

	// Send connection request to server
	connectMsg := NewConnect()
	connectMsg.ConnID = 0
	connectMsg.SeqNum = 0
	m_msg, marshal_err := json.Marshal(connectMsg)
	current_client.PrintError(marshal_err)
	_, write_msg_err := current_conn.Write(m_msg)
	current_client.PrintError(write_msg_err)

	// Block until we get a connection ack back
	connectAck := <- current_client.connectCh

	// Then return the appropriate client if we get an ack back for the connection request
	if connectAck == nil {
		return nil, errors.New("Could not connect!")
	} else {
		if current_client.verbose {
			fmt.Printf("Client side constructor: succeeded in getting a connect ack back! connID is %d\n", connectAck.ConnID)
		}
		current_client.connID = connectAck.ConnID
		current_client.lowestUnackSN++
		return current_client, nil
	}
}

func (c *client) ConnID() int {
	return c.connID
}

func (c *client) Read() ([]byte, error) {
	if c.verbose {
		fmt.Println("Client side Read(): Client's Read() API method was called")
	}
	msg := <- c.intermedReadCh
	if msg == nil {
		fmt.Println("Client side Read(): A nil message was pulled out of the intermedReadCh")
		return nil, errors.New("")
	}
	if c.verbose {
		fmt.Println("Client side Read(): %s was read.", msg.Payload)
	}
	return msg.Payload, nil
}

func (c *client) Write(payload []byte) error {
	if c.verbose {
		fmt.Printf("Client side Write(): Client's Write() API method was called, we are writing %s with seqnum %d\n", string(payload), c.currWriteSN)
	}
	if (!c.isClosed) {
		msg := NewData(c.connID, c.currWriteSN, payload)
		c.writeCh <- msg
		if c.verbose {
			fmt.Printf("Client side Write(): writeCh is length %d\n", len(c.writeCh))
		}
		c.currWriteSN++
	}
	return nil
}

func (c *client) Close() error {
	if c.verbose {
		fmt.Println("Client side Close(): Client's Close() API method was called")
	}
	c.isClosed = true			// TODO: Do we need a channel as opposed to a bool?
	return nil
}

// ===================== GO ROUTINES ================================

func (c *client) master() {
	// We want to exit this for loop once all pending messages to the server
	// have been sent and acknowledged.
	if c.verbose {
		fmt.Println("Client side master(): is running")
	}
	for {
		select {
		// Check to see if Close() has been called
		case <- c.closeCh:
			if c.verbose {
				fmt.Println("Client side master(): Client is in closeCh")
			}
			c.closeCh <- 1
			return
		case msg := <- c.readCh:
			if c.verbose {
				fmt.Println("Client side master(): Client is in readCh")
			}
			currentSN := msg.SeqNum
			switch msg.Type {
			case MsgAck:
				if c.verbose {
					fmt.Printf("Client side master(): Client received MsgAck w/ seqnum %d\n", currentSN)
				}
				// If this is a acknowledgement for a connection request
				if currentSN == 0 {
					c.connectCh <- msg
				} else {
					if _, ok := c.dataWindow[currentSN]; ok {
						delete(c.dataWindow,currentSN)

						// If we received an ack for the oldest unacked data msg
						if currentSN == c.lowestUnackSN {
							if len(c.dataWindow) == 0 {
								c.lowestUnackSN = c.findNewMin(c.dataWindow)
							} else {
								c.lowestUnackSN++
							}
						}

						// if Close() is called and writeCh is empty and dataWindow is empty
						if (c.isClosed && len(c.writeCh) == 0 && len(c.dataWindow) == 0) {
							c.closeCh <- 1
						}
					}
				}
			case MsgData:
				if c.verbose {
					fmt.Printf("Client side master(): Client received MsgData and data is %s\n", string(msg.Payload))
					fmt.Printf("Client side master(): Client has received the message it expects. CurrentSN is %d and expectedSN is %d\n", currentSN, c.expectedSN)
				}
				// Drop any message that isn't the expectedSN
				if (currentSN == c.expectedSN) {
					if c.verbose {
						fmt.Printf("Client side master(): got a data message and it's the expectedSn\n")
					}
					c.intermedReadCh <- msg
					c.expectedSN++
					c.numEpochs = 0

					ackMsg := NewAck(c.connID, currentSN)
					c.sendMessage(ackMsg)

					oldestAckSN := c.findNewMin(c.ackWindow)
					delete(c.ackWindow, oldestAckSN)
					c.ackWindow[currentSN] = ackMsg
				}
			}

		case msg := <- c.writeCh:
			if c.verbose {
				fmt.Println("Client side master(): Client is in writeCh")
			}
			msgSent := false
			// If message cannot be sent, then keep trying until it is sent
			for (!msgSent) {
				// Check if we can write the message based on SN
				if c.verbose {
					fmt.Printf("Client side master(): trying to send msg in writeCh with seqnum %d\n", msg.SeqNum)
				}
				if (c.lowestUnackSN <= msg.SeqNum && msg.SeqNum <= c.lowestUnackSN + c.windowSize && len(c.dataWindow) < c.windowSize) {
					if c.verbose {
						fmt.Printf("Client side master(): seq num is %d and lowestunackSN is %d\n", msg.SeqNum,c.lowestUnackSN)
					}
					// c.sendMessage(msg)
					m_msg, marshal_err := json.Marshal(msg)
					c.PrintError(marshal_err)
					_, write_msg_err := c.connection.Write(m_msg)
					if write_msg_err != nil {
						fmt.Fprintf(os.Stderr, "Client failed to write to the server. Exit code 1.", write_msg_err)
						os.Exit(1)
					}
					if c.verbose {
						fmt.Printf("Client side master(): just sent the msg to the server\n")
					}
					msgSent = true

					// Change the data window to include sent message
					c.dataWindow[msg.SeqNum] = msg
				}
			}

		case <- c.epochCh:
			if c.verbose {
				fmt.Println("Client side master(): Client is in epochCh")
			}
			c.epochHelper()
		}
	}
}

func (c *client) read() {
	for {
		select {
			case <- c.closeCh:
				if c.verbose {
					fmt.Printf("Client side read(): closeCh has something\n")
				}
				c.closeCh <- 1 					// very hacky: we are putting it back in order to close the other go routines
				return
			default:
				if c.verbose {
					fmt.Printf("Client side read(): read in a message\n")
				}
				buff := make([]byte, 1500)
				num_bytes_received, _, received_err := c.connection.ReadFromUDP(buff[0:])
				if received_err != nil {
					fmt.Fprintf(os.Stderr, "Client failed to read from the server. Exit code 2.", received_err)
					os.Exit(2)
				}

				received_msg := Message{}
				unmarshal_err := json.Unmarshal(buff[0:num_bytes_received], &received_msg)
				c.PrintError(unmarshal_err)
				if c.verbose {
					fmt.Printf("Client side read(): Sequence number of message being put into readCh is: %d\n", received_msg.SeqNum)
				}
				c.readCh <- &received_msg
		}
	}
}

func (c *client) epoch() {
	c.epochCh = time.NewTicker(time.Duration(c.epochMilli) * time.Millisecond).C

	for {
		select {
		case <- c.closeCh:
			if c.verbose {
				fmt.Printf("Client side epoch(): closeCh has something\n")
			}
			c.closeCh <- 1
			return
		default:
			// if c.verbose {
			// 	fmt.Printf("Client side epoch(): putting ticker \n")
			// }
			// Once an epoch has been reached, epochCh is notified
			// c.epochCh = time.NewTicker(time.Duration(c.epochMilli) * time.Millisecond).C
		}
	}
}

// ===================== HELPER FUNCTIONS ================================

func (c *client) findNewMin(currentMap map[int]*Message) int {
	currentLowest := MaxInt
	for key, _ := range currentMap {
		if key < currentLowest {
			currentLowest = key
		}
	}
	return currentLowest
}

func (c *client) epochHelper() {
	if c.verbose {
		fmt.Printf("Client side epochHelper: WE ARE IN EPOCHHELPER")
	}
	// If client's connection request hasn't been acknowledged,
	// resent the connection request
	if (c.connID <= 0) {
		if c.verbose {
			fmt.Printf("Client side epochHelper: c.connID is less than or equal to 0")
		}

		if c.numEpochs < c.epochLimit {
			if c.verbose {
				fmt.Printf("Client side epochHelper: we are trying to send another connect message")
			}
			connectMsg := NewConnect()
			connectMsg.ConnID = 0
			connectMsg.SeqNum = 0
			c.sendMessage(connectMsg)
		} else {
			c.Close()
		}

		return
	}

	// If connection request is sent and acknowledged, but no data
	// messages have been received, then send an acknowledgement with
	// seqence number 0
	if c.expectedSN == 0 {
		if c.verbose {
			fmt.Printf("Client side epochHelper: c.expectedSN == 0")
		}

		ackMsg := NewAck(c.connID, 0)
		c.sendMessage(ackMsg)

		c.ackWindow[0] = ackMsg
		return
	}

	// For each data message that has been sent but not yet acknowledged,
	// resend the data message
	for _, value := range c.dataWindow {
		if c.verbose {
			fmt.Printf("Client side epochHelper: we are resending data from dataWindow for SN %d\n", value)
		}
		c.sendMessage(value)
	}

	// Resend an acknowledgement message for each of the last w (or fewer)
	// distinct data messages that have been received
	for _, value := range c.ackWindow {
		if c.verbose {
			fmt.Printf("Client side epochHelper: we are resending acks from ackWindow for SN %d\n", value.SeqNum)
		}
		c.sendMessage(value)
	}

	c.numEpochs++
}

func (c *client) sendMessage(msg *Message) {
	m_msg, marshal_err := json.Marshal(msg)
	c.PrintError(marshal_err)
	_, write_msg_err := c.connection.Write(m_msg)
	if write_msg_err != nil {
		fmt.Fprintf(os.Stderr, "Client failed to write to the server. Exit code 1.", write_msg_err)
		os.Exit(1)
	}
}

func (c *client) PrintError(err error) {
	if err != nil {
		fmt.Println("The error is: ", err)
	}
}

func (c *client) ReturnError(err error, line int) error {
	if err != nil {
		c.PrintError(err)
		return err
	}
	return errors.New("Error")
}
