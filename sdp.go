package main

const (
	sdpBase   = "v=0\r\no=sipbench 1719874986 1719874987 IN IP4 %s\r\ns=sipbench\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 8\r\na=rtpmap:8 PCMA/8000\r\n"
	sdpSIPREC = "v=0\r\no=sipbench 1719874986 1719874987 IN IP4 %s\r\ns=sipbench\r\nc=IN IP4 %s\r\nt=0 0\r\n"
)
