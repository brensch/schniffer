// Initialize the map
const map = L.map('map').setView([39.8283, -98.5795], 4); // Center of US

// Add OpenStreetMap tiles
L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
}).addTo(map);

let markers = [];
let currentData = null;

// Custom icons for different providers
const recreationIcon = L.divIcon({
    className: 'custom-div-icon',
    html: '<div style="background-color: #27ae60; color: white; border-radius: 50%; width: 20px; height: 20px; display: flex; align-items: center; justify-content: center; font-weight: bold; border: 2px solid white; box-shadow: 0 2px 4px rgba(0,0,0,0.3);">R</div>',
    iconSize: [20, 20],
    iconAnchor: [10, 10]
});

const californiaIcon = L.divIcon({
    className: 'custom-div-icon',
    html: '<div style="background-color: #e74c3c; color: white; border-radius: 50%; width: 20px; height: 20px; display: flex; align-items: center; justify-content: center; font-weight: bold; border: 2px solid white; box-shadow: 0 2px 4px rgba(0,0,0,0.3);">C</div>',
    iconSize: [20, 20],
    iconAnchor: [10, 10]
});

// Create cluster icon with count
function createClusterIcon(count) {
    const size = Math.min(Math.max(20 + Math.log10(count) * 10, 25), 50);
    return L.divIcon({
        className: 'custom-div-icon',
        html: `<div style="background-color: #3498db; color: white; border-radius: 50%; width: ${size}px; height: ${size}px; display: flex; align-items: center; justify-content: center; font-weight: bold; border: 3px solid white; box-shadow: 0 2px 6px rgba(0,0,0,0.4); font-size: ${Math.min(size/3, 14)}px;">${count}</div>`,
        iconSize: [size, size],
        iconAnchor: [size/2, size/2]
    });
}

// Load campgrounds for current viewport
async function loadViewportData() {
    const bounds = map.getBounds();
    const zoom = map.getZoom();
    
    const viewport = {
        north: bounds.getNorth(),
        south: bounds.getSouth(),
        east: bounds.getEast(),
        west: bounds.getWest(),
        zoom: zoom
    };
    
    try {
        const response = await fetch('/api/viewport', {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify(viewport)
        });
        
        const result = await response.json();
        currentData = result;
        renderMarkersFromViewport(result);
        updateStatsFromViewport(result);
    } catch (error) {
        console.error('Failed to load viewport data:', error);
        document.getElementById('stats').textContent = 'Failed to load campgrounds';
    }
}

function updateStatsFromViewport(result) {
    if (result.type === 'clusters') {
        const totalCount = result.data.reduce((sum, cluster) => sum + cluster.count, 0);
        document.getElementById('stats').textContent = 
            `${totalCount} campgrounds in ${result.data.length} clusters (zoom in for details)`;
    } else {
        const recCount = result.data.filter(c => c.provider === 'recreation_gov').length;
        const calCount = result.data.filter(c => c.provider === 'reservecalifornia').length;
        document.getElementById('stats').textContent = 
            `${result.data.length} campgrounds visible (${recCount} Recreation.gov, ${calCount} ReserveCalifornia)`;
    }
}

function renderMarkersFromViewport(result) {
    // Clear existing markers
    markers.forEach(marker => map.removeLayer(marker));
    markers = [];
    
    // Get filter states
    const showRecreation = document.getElementById('recreation_gov').checked;
    const showCalifornia = document.getElementById('reservecalifornia').checked;
    const searchTerm = document.getElementById('search').value.toLowerCase();
    
    if (result.type === 'clusters') {
        // Render clusters
        result.data.forEach(cluster => {
            const marker = L.marker([cluster.lat, cluster.lon], { 
                icon: createClusterIcon(cluster.count) 
            }).bindPopup(`
                <div class="custom-popup">
                    <div class="popup-title">${cluster.count} Campgrounds</div>
                    <div style="margin-top: 0.5rem;">
                        ${cluster.names.slice(0, 3).map(name => `<div style="font-size: 0.9rem; margin: 0.2rem 0;">• ${name}</div>`).join('')}
                        ${cluster.names.length > 3 ? `<div style="font-size: 0.8rem; color: #666; margin-top: 0.3rem;">... and ${cluster.count - 3} more</div>` : ''}
                        <div style="font-size: 0.8rem; color: #666; margin-top: 0.5rem;">Zoom in to see individual campgrounds</div>
                    </div>
                </div>
            `).addTo(map);
            
            markers.push(marker);
        });
    } else {
        // Render individual campgrounds
        const filteredCampgrounds = result.data.filter(campground => {
            const matchesProvider = 
                (campground.provider === 'recreation_gov' && showRecreation) ||
                (campground.provider === 'reservecalifornia' && showCalifornia);
            
            const matchesSearch = !searchTerm || 
                campground.name.toLowerCase().includes(searchTerm);
            
            return matchesProvider && matchesSearch;
        });
        
        filteredCampgrounds.forEach(campground => {
            const icon = campground.provider === 'recreation_gov' ? recreationIcon : californiaIcon;
            
            const marker = L.marker([campground.lat, campground.lon], { icon })
                .bindPopup(`
                    <div class="custom-popup">
                        <div class="popup-title">${campground.name}</div>
                        <div class="popup-provider ${campground.provider}">${campground.provider.replace('_', ' ')}</div>
                        <div class="popup-coordinates">
                            ${campground.lat.toFixed(4)}, ${campground.lon.toFixed(4)}
                        </div>
                    </div>
                `)
                .addTo(map);
            
            markers.push(marker);
        });
    }
}

// Event listeners
map.on('moveend zoomend', loadViewportData);

document.getElementById('recreation_gov').addEventListener('change', () => {
    if (currentData) renderMarkersFromViewport(currentData);
});
document.getElementById('reservecalifornia').addEventListener('change', () => {
    if (currentData) renderMarkersFromViewport(currentData);
});

let searchTimeout;
document.getElementById('search').addEventListener('input', () => {
    clearTimeout(searchTimeout);
    searchTimeout = setTimeout(() => {
        if (currentData) renderMarkersFromViewport(currentData);
    }, 300);
});

// Load initial data
loadViewportData();
