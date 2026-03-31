'use strict';

class Dashboard {
    constructor() {
        this.pollInterval = 3000;
        this.timer = null;
        this.token = null;
        this.init();
    }

    init() {
        // Bind login
        document.getElementById('login-btn').addEventListener('click', () => this.login());
        document.getElementById('login-password').addEventListener('keydown', e => {
            if (e.key === 'Enter') this.login();
        });

        // Bind create room
        document.getElementById('create-room-btn').addEventListener('click', () => this.createRoom());
        document.getElementById('new-room-name').addEventListener('keydown', e => {
            if (e.key === 'Enter') this.createRoom();
        });

        // Bind logout
        document.getElementById('logout-btn').addEventListener('click', () => this.logout());

        // Check existing session
        this.checkSession();
    }

    async checkSession() {
        try {
            const resp = await fetch('/api/auth');
            if (resp.ok) {
                this.showDashboard();
                return;
            }
        } catch {}
        this.showLogin();
    }

    async login() {
        const input = document.getElementById('login-password');
        const errEl = document.getElementById('login-error');
        const password = input.value.trim();
        errEl.textContent = '';

        if (!password) { input.focus(); return; }

        try {
            const resp = await fetch('/api/login', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ password }),
            });

            if (!resp.ok) {
                errEl.textContent = 'Invalid password';
                input.value = '';
                input.focus();
                return;
            }

            const data = await resp.json();
            this.token = data.token;
            this.showDashboard();
        } catch (err) {
            errEl.textContent = 'Connection failed';
        }
    }

    logout() {
        document.cookie = 'stun_max_token=; Max-Age=0; path=/';
        this.token = null;
        if (this.timer) clearInterval(this.timer);
        this.showLogin();
    }

    showLogin() {
        document.getElementById('login-page').style.display = 'flex';
        document.getElementById('dashboard-page').style.display = 'none';
        document.getElementById('login-password').value = '';
        document.getElementById('login-error').textContent = '';
        document.getElementById('login-password').focus();
    }

    showDashboard() {
        document.getElementById('login-page').style.display = 'none';
        document.getElementById('dashboard-page').style.display = 'block';
        this.refresh();
        if (this.timer) clearInterval(this.timer);
        this.timer = setInterval(() => this.refresh(), this.pollInterval);
    }

    async apiFetch(url, opts = {}) {
        const resp = await fetch(url, opts);
        if (resp.status === 401) {
            this.logout();
            return null;
        }
        return resp;
    }

    async refresh() {
        try {
            const resp = await this.apiFetch('/api/stats');
            if (!resp) return;
            if (!resp.ok) {
                this.setStatus('error', `HTTP ${resp.status}`);
                return;
            }
            const stats = await resp.json();
            document.getElementById('room-count').textContent = stats.total_rooms;
            document.getElementById('peer-count').textContent = stats.total_peers;
            this.render(stats.rooms || []);
            this.setStatus('online', 'Connected');
        } catch {
            this.setStatus('error', 'Offline');
        }
    }

    setStatus(state, text) {
        const dot = document.getElementById('status-dot');
        const label = document.getElementById('status-text');
        dot.style.background = state === 'online' ? '#00c853' : '#ff4444';
        label.textContent = text;
    }

    async createRoom() {
        const nameEl = document.getElementById('new-room-name');
        const passEl = document.getElementById('new-room-pass');
        const name = nameEl.value.trim();
        if (!name) { nameEl.focus(); return; }

        try {
            const resp = await this.apiFetch('/api/rooms', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ name, password: passEl.value }),
            });
            if (resp && resp.ok) {
                nameEl.value = '';
                passEl.value = '';
                this.refresh();
            }
        } catch {}
    }

    async deleteRoom(name) {
        if (!confirm(`Delete room "${name}" and disconnect all peers?`)) return;
        try {
            await this.apiFetch(`/api/rooms?name=${encodeURIComponent(name)}`, { method: 'DELETE' });
            this.refresh();
        } catch {}
    }

    async banClient(room, clientId) {
        if (!confirm(`Ban client ${clientId} from room "${room}"?`)) return;
        try {
            await this.apiFetch('/api/rooms/ban', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ room, client_id: clientId }),
            });
            this.refresh();
        } catch {}
    }

    async unbanClient(room, clientId) {
        try {
            await this.apiFetch('/api/rooms/unban', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ room, client_id: clientId }),
            });
            this.refresh();
        } catch {}
    }

    render(rooms) {
        let directCount = 0, relayCount = 0;
        for (const room of rooms) {
            for (const p of room.peers) {
                if (p.status === 'direct') directCount++;
                if (p.status === 'relay') relayCount++;
            }
        }

        document.getElementById('direct-count').textContent = directCount;
        document.getElementById('relay-count').textContent = relayCount;

        const container = document.getElementById('rooms-list');
        const noRooms = document.getElementById('no-rooms');

        if (rooms.length === 0) {
            container.innerHTML = '';
            noRooms.style.display = 'block';
            return;
        }
        noRooms.style.display = 'none';

        container.innerHTML = rooms.map(room => `
            <div class="room-card">
                <div class="room-header">
                    <span class="room-name">${this.esc(room.name)}</span>
                    <span class="room-badge ${room.protected ? 'badge-protected' : 'badge-open'}">
                        ${room.protected ? '🔒 Protected' : 'Open'}
                    </span>
                    <span class="room-traffic">${this.formatBytes(room.bytes_relayed || 0)} relayed</span>
                    <span class="room-peer-count">${room.peers.length} peer${room.peers.length !== 1 ? 's' : ''}</span>
                    <button class="room-delete" onclick="app.deleteRoom('${this.esc(room.name)}')">Delete</button>
                </div>
                ${room.peers.length > 0 ? `
                <table class="peer-table">
                    <thead><tr><th>ID</th><th>Name</th><th>Connection</th><th>Endpoint</th><th></th></tr></thead>
                    <tbody>
                        ${room.peers.map(p => `
                            <tr>
                                <td class="peer-id">${this.esc(p.id)}</td>
                                <td class="peer-name-cell">${this.esc(p.name || '-')}</td>
                                <td><span class="mode-badge mode-${p.status}">
                                    ${p.status === 'direct' ? '⚡ P2P' : p.status === 'relay' ? '🔄 RELAY' : '⏳ ...'}
                                </span></td>
                                <td class="peer-endpoint">${this.esc(p.endpoint || '-')}</td>
                                <td><button class="peer-ban-btn" onclick="app.banClient('${this.esc(room.name)}','${this.esc(p.id)}')">Ban</button></td>
                            </tr>
                        `).join('')}
                    </tbody>
                </table>` : '<div class="empty-state" style="padding:20px">No peers connected</div>'}
                ${room.blacklist && room.blacklist.length > 0 ? `
                <div class="banned-section">
                    <div class="banned-title">Banned (${room.blacklist.length})</div>
                    <div class="banned-list">
                        ${room.blacklist.map(id => `
                            <span class="banned-entry">
                                <span class="banned-id">${this.esc(id)}</span>
                                <button class="peer-unban-btn" onclick="app.unbanClient('${this.esc(room.name)}','${this.esc(id)}')">Unban</button>
                            </span>
                        `).join('')}
                    </div>
                </div>` : ''}
            </div>
        `).join('');
    }

    formatBytes(bytes) {
        if (bytes === 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
    }

    esc(str) {
        const d = document.createElement('div');
        d.textContent = str;
        return d.innerHTML;
    }
}

const app = new Dashboard();
