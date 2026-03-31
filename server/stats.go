package main

import (
	"time"
)

var serverStartTime = time.Now()

type ServerStats struct {
	Uptime     string     `json:"uptime"`
	TotalRooms int        `json:"total_rooms"`
	TotalPeers int        `json:"total_peers"`
	TotalBytes int64      `json:"total_bytes_relayed"`
	Rooms      []RoomInfo `json:"rooms"`
}

func (h *Hub) getStats() ServerStats {
	rooms := h.getRoomsInfo()
	var totalPeers int
	var totalBytes int64
	for _, r := range rooms {
		totalPeers += len(r.Peers)
		totalBytes += r.BytesRelayed
	}
	return ServerStats{
		Uptime:     time.Since(serverStartTime).Round(time.Second).String(),
		TotalRooms: len(rooms),
		TotalPeers: totalPeers,
		TotalBytes: totalBytes,
		Rooms:      rooms,
	}
}
