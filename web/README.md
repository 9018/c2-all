# emp3r0r Web Panel

Web-based management interface for emp3r0r C2 framework.

## Features

- 🔐 Token-based authentication
- 📊 Real-time dashboard with agent statistics
- 🖥️ Agent management (list, select, forget)
- 💻 Interactive command console
- 📦 Module browser and execution
- 🔄 WebSocket real-time updates

## Requirements

- Node.js 18+ (for development)
- emp3r0r C2 server running with Web API enabled

## Quick Start

### Development

```bash
# Install dependencies
npm install

# Start development server
npm run dev

# Access panel at http://localhost:5173
```

### Production Build

```bash
# Build for production
npm run build

# Output will be in dist/ directory
```

## Configuration

The web panel connects to the emp3r0r C2 server's Web API.

### Default Ports

| Service | Port | Protocol |
|---------|------|----------|
| Web UI (Dev) | 5173 | HTTP |
| Web UI (Prod) | 9443 | HTTPS |
| API Proxy | - | HTTPS → localhost:9443 |

### Authentication

The panel uses token-based authentication. The token is:
1. Generated automatically by the C2 server
2. Displayed in server logs on startup
3. Stored in `~/.emp3r0r/web_token.txt`

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                    Web Browser                       │
│  ┌───────────────────────────────────────────────┐  │
│  │              React Frontend                    │  │
│  │  - Dashboard    - Agent List                   │  │
│  │  - Console      - Module Panel                 │  │
│  └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────┘
                          │
                          │ HTTPS (JSON API + WebSocket)
                          ▼
┌─────────────────────────────────────────────────────┐
│              emp3r0r C2 Server (Web API)            │
│  - /api/health         Health check                 │
│  - /api/agents         List agents                  │
│  - /api/command        Send commands                │
│  - /api/modules        List modules                 │
│  - /api/ws             WebSocket                    │
└─────────────────────────────────────────────────────┘
```

## API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | /api/health | Health check |
| GET | /api/agents | List connected agents |
| POST | /api/agents/active | Set active agent |
| POST | /api/agents/forget | Remove agent |
| POST | /api/command | Send command to agent |
| GET | /api/modules | List available modules |
| WS | /api/ws | WebSocket for real-time updates |

## Development

### Project Structure

```
web/
├── src/
│   ├── components/     # React components
│   ├── hooks/          # Custom hooks
│   ├── lib/            # Utilities (API client, WebSocket)
│   ├── stores/         # Zustand state management
│   ├── types/          # TypeScript types
│   └── main.tsx        # Entry point
├── public/             # Static assets
├── package.json
├── vite.config.ts
└── tailwind.config.js
```

### Adding New Features

1. Define types in `src/types/`
2. Add API methods in `src/lib/api.ts`
3. Create components in `src/components/`
4. Update stores if needed

## Security Notes

- The WebSocket server allows all origins in development
- In production, configure proper CORS restrictions
- Token should be kept secret and rotated periodically
- All API endpoints require authentication

## Troubleshooting

### Connection Issues

1. Ensure C2 server is running
2. Check if port 9443 is accessible
3. Verify the access token is correct
4. Check browser console for errors

### Build Errors

```bash
# Clear cache and reinstall
rm -rf node_modules package-lock.json
npm install
```

## License

Part of emp3r0r C2 framework.
