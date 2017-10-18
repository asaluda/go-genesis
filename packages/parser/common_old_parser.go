package parser

import (
	"bytes"
	"fmt"

	"github.com/EGaaS/go-egaas-mvp/packages/consts"
	"github.com/EGaaS/go-egaas-mvp/packages/converter"

	log "github.com/sirupsen/logrus"
)

func ParseOldTransaction(buffer *bytes.Buffer) ([][]byte, error) {
	var transSlice [][]byte

	transSlice = append(transSlice, []byte{})                                                  // hash placeholder
	transSlice = append(transSlice, []byte{})                                                  // type placeholder
	transSlice = append(transSlice, converter.Int64ToByte(converter.BinToDec(buffer.Next(4)))) // time

	if buffer.Len() == 0 {
		log.Error("buffer is empty, while parsing old transaction")
		return transSlice, fmt.Errorf("incorrect tx")
	}

	for buffer.Len() > 0 {
		length, err := converter.DecodeLengthBuf(buffer)
		if err != nil {
			log.Error("decoding length, while parsing old transaction")
			return nil, err
		}

		if length > buffer.Len() || length > consts.MAX_TX_SIZE {
			log.WithFields(log.Fields{"size": buffer.Len(), "max_size": consts.MAX_TX_SIZE, "decoded_size": length}).Error("bad transaction")
			return nil, fmt.Errorf("bad transaction")
		}

		if length > 0 {
			transSlice = append(transSlice, buffer.Next(length))
			continue
		}

		if length == 0 && buffer.Len() > 0 {
			transSlice = append(transSlice, []byte{})
			continue
		}

		if length == 0 {
			log.Error("bad transaction")
			break
		}
	}

	return transSlice, nil
}
