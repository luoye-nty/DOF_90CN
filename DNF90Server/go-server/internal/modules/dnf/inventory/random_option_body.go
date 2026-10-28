package inventory

func buildRandomOptionStatusAck(success bool) []byte {
	if success {
		return []byte{1}
	}
	return []byte{0}
}

// buildRandomOptionResetErrorAck is the current NoPack class1/op455 failure
// body. The current client's class1 dispatcher reads the leading result byte
// and, when it is zero, exactly one further error-code byte; the op455 result
// handler then paints 7/0x15/0x13/4/0x11/0x17 as messages.
func buildRandomOptionResetErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}
