package consensus

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	//"github.com/dfinlab/meter/types"
	"net"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/rlp"
)

// ****** test code ***********
type PMessage struct {
	Round                   uint64
	MsgType                 byte
	QC_height               uint64
	QC_round                uint64
	Block_height            uint64
	Block_round             uint64
	Block_parent_height     uint64
	Block_parent_round      uint64
	Block_justify_QC_height uint64
	Block_justify_QC_round  uint64
}

// String returns a string representation.
func (m *PMessage) String() string {
	return fmt.Sprintf("PMessage: Round(%v), MsgtType(%v), QC_height(%v), QC_round(%v), Block_height(%v), Block_round(%v), Block_parent_height(%v), Block_parent_round(%v), Block_justify_QC_height(%v), Block_justify_QC_round(%v)",
		m.Round, m.MsgType, m.QC_height, m.QC_round, m.Block_height, m.Block_round, m.Block_parent_height,
		m.Block_parent_round, m.Block_justify_QC_height, m.Block_justify_QC_round)
}

// check a pmBlock is the extension of b_locked, max 10 hops
func (p *Pacemaker) IsExtendedFromBLocked(b *pmBlock) bool {

	i := int(0)
	tmp := b
	for i < 10 {
		if tmp == p.blockLocked {
			return true
		}
		tmp = tmp.Parent
		i++
	}
	return false
}

func (p *Pacemaker) Send(cm CommitteeMember, m []byte) error {
	myNetAddr := p.csReactor.curCommittee.Validators[p.csReactor.curCommitteeIndex].NetAddr
	payload := map[string]interface{}{
		"message": hex.EncodeToString(m),
		"peer_ip": myNetAddr.IP.String(),
		//"peer_id":   string(myNetAddr.ID),
		"peer_port": string(myNetAddr.Port),
	}

	jsonStr, err := json.Marshal(payload)
	if err != nil {
		panic("Failed to marshal message dict to json string")
		return err
	}

	var netClient = &http.Client{
		Timeout: time.Second * 2,
	}
	resp, err := netClient.Post("http://"+cm.NetAddr.IP.String()+":8670/peer", "application/json", bytes.NewBuffer(jsonStr))
	if err != nil {
		p.csReactor.logger.Error("Failed to send message to peer", "peer", cm.NetAddr.IP.String(), "err", err)
		return err
	}
	p.csReactor.logger.Info("Sent consensus message to peer", "peer", cm.NetAddr.IP.String(), "size", len(m))
	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	return nil
}

func (p *Pacemaker) sendMsg(round uint64, msgType byte, qc *QuorumCert, b *pmBlock) error {
	var m *PMessage

	if b == nil {
		m = &PMessage{
			Round:     round,
			MsgType:   msgType,
			QC_height: qc.QCHeight,
			QC_round:  qc.QCRound,
		}
	} else {
		m = &PMessage{
			Round:                   round,
			MsgType:                 msgType,
			QC_height:               qc.QCHeight,
			QC_round:                qc.QCRound,
			Block_height:            b.Height,
			Block_round:             b.Round,
			Block_parent_height:     b.Parent.Height,
			Block_parent_round:      b.Parent.Round,
			Block_justify_QC_height: b.Justify.QCHeight,
			Block_justify_QC_round:  b.Justify.QCRound,
		}
	}

	msgByte, err := rlp.EncodeToBytes(m)
	if err != nil {
		fmt.Println("panic:", err)
		panic("message encode failed")
	}

	to := p.csReactor.getRoundProposer(int(round))
	p.Send(to, msgByte)

	p.csReactor.logger.Info("Sent message", "message", m.String())
	return nil
}

// everybody in committee include myself
func (p *Pacemaker) broadcastMsg(round uint64, msgType byte, qc *QuorumCert, b *pmBlock) error {
	m := &PMessage{
		Round:                   round,
		MsgType:                 msgType,
		QC_height:               qc.QCHeight,
		QC_round:                qc.QCRound,
		Block_height:            b.Height,
		Block_round:             b.Round,
		Block_parent_height:     b.Parent.Height,
		Block_parent_round:      b.Parent.Round,
		Block_justify_QC_height: b.Justify.QCHeight,
		Block_justify_QC_round:  b.Justify.QCRound,
	}

	msgByte, err := rlp.EncodeToBytes(m)
	if err != nil {
		fmt.Println("panic:", err)
		panic("message encode failed")
	}

	// send myself first, a little bit ugly, but ...
	myNetAddr := p.csReactor.curCommittee.Validators[p.csReactor.curCommitteeIndex].NetAddr
	for _, cm := range p.csReactor.curActualCommittee {
		if bytes.Equal(myNetAddr.IP, cm.NetAddr.IP) == true {
			p.Send(cm, msgByte)
			break
		}
	}
	for _, cm := range p.csReactor.curActualCommittee {
		if bytes.Equal(myNetAddr.IP, cm.NetAddr.IP) == false {
			p.Send(cm, msgByte)
		}
	}

	p.csReactor.logger.Info("Beoadcasted message", "message", m.String())
	return nil
}

// find out b b' b"
func (p *Pacemaker) AddressBlock(height uint64, round uint64) *pmBlock {
	if (p.proposalMap[height] != nil) && (p.proposalMap[height].Height == height) && (p.proposalMap[height].Round == round) {
		p.csReactor.logger.Info("addressed block", "height", height, "round", round)
		return p.proposalMap[height]
	}

	p.csReactor.logger.Info("Could not find out block", "height", height, "round", round)
	return nil
}

func (p *Pacemaker) decodeMsg(msg []byte) (error, *PMessage) {
	m := &PMessage{}
	if err := rlp.DecodeBytes(msg, m); err != nil {
		fmt.Println("Decode message failed", err)
		return errors.New("decode message failed"), &PMessage{}
	}
	return nil, m
}

func (p *Pacemaker) Receive(m *PMessage) error {

	p.csReactor.logger.Info("Receives a pacemaker message", "message", m.String())
	// receives proposal message, block is new one. parent is one of (b,b',b")
	if m.MsgType == PACEMAKER_MSG_PROPOSAL {
		parent := p.AddressBlock(m.Block_parent_height, m.Block_parent_round)
		if parent == nil {
			return errors.New("can not address parent")
		}

		qcNode := p.AddressBlock(m.Block_justify_QC_height, m.Block_justify_QC_round)
		if qcNode == nil {
			return errors.New("can not address qcNode")
		}

		justify := &QuorumCert{
			QCHeight: m.Block_justify_QC_height,
			QCRound:  m.Block_justify_QC_round,
			QCNode:   qcNode,
		}

		p.proposalMap[m.Block_height] = &pmBlock{
			Height:  m.Block_height,
			Round:   m.Block_round,
			Parent:  parent,
			Justify: justify,
		}
		return p.OnReceiveProposal(p.proposalMap[m.Block_height])

	} else if m.MsgType == PACEMAKER_MSG_VOTE {
		// must be in (b, b', b")
		b := p.AddressBlock(m.Block_height, m.Block_round)
		if b == nil {
			return errors.New("can not address block")
		}

		if (b.Parent.Height != m.Block_parent_height) ||
			(b.Parent.Round != m.Block_parent_round) ||
			(b.Justify.QCHeight != m.Block_justify_QC_height) ||
			(b.Justify.QCRound != m.Block_justify_QC_round) {
			return errors.New("mismatch, something wrong")
		}
		return p.OnReceiveVote(b)
	} else if m.MsgType == PACEMAKER_MSG_NEWVIEW {
		qcNode := p.AddressBlock(m.QC_height, m.QC_round)
		if qcNode == nil {
			return errors.New("can not address qcNode")
		}
		qc := &QuorumCert{
			QCHeight: m.QC_height,
			QCRound:  m.QC_round,
			QCNode:   qcNode,
		}
		return p.OnRecieveNewView(qc)
	} else {
		return errors.New("unknown pacemaker message type")
	}
}

func (p *Pacemaker) receivePacemakerMsg(w http.ResponseWriter, r *http.Request) {

	defer r.Body.Close()
	var params map[string]string
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		p.csReactor.logger.Error("%v\n", err)
		respondWithJson(w, http.StatusBadRequest, "Invalid request payload")
		return
	}
	peerIP := net.ParseIP(params["peer_ip"])

	msgByteSlice, _ := hex.DecodeString(params["message"])
	err, message := p.decodeMsg(msgByteSlice)
	if err != nil {
		p.csReactor.logger.Error("message decode error", err)
		panic("message decode error")
	} else {
		p.csReactor.logger.Info("receive pacemaker msg from", "IP", peerIP, "msgType", message.MsgType)
		p.Receive(message)
	}
	respondWithJson(w, http.StatusOK, map[string]string{"result": "success"})

}