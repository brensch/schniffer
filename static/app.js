// Get user token from URL parameters
const urlParams = new URLSearchParams(window.location.search);
const userToken = urlParams.get('user');

// Initialize the map
const map = L.map('map').setView([39.8283, -98.5795], 4); // Center of US

// Add OpenStreetMap tiles
L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
}).addTo(map);

let markers = [];
let currentData = null;

// Custom icons for different providers - 🐽 emoji for all sites
const recreationIcon = L.divIcon({
    className: 'custom-div-icon',
    html: '<div style="font-size: 24px;">🐽</div>',
    iconSize: [30, 30],
    iconAnchor: [15, 15]
});

const californiaIcon = L.divIcon({
    className: 'custom-div-icon',
    html: '<div style="font-size: 24px;">🐽</div>',
    iconSize: [30, 30],
    iconAnchor: [15, 15]
});

// Create cluster icon - 🐽 emoji for aggregate view with count below
function createClusterIcon(count) {
    const size = Math.min(Math.max(25 + Math.log10(count) * 15, 30), 70);
    const fontSize = Math.min(size/1.3, 40);
    const numberSize = Math.min(size/4, 12);
    return L.divIcon({
        className: 'custom-div-icon',
        html: `<div style="display: flex; flex-direction: column; align-items: center; justify-content: center; font-family: 'Epilogue', sans-serif;">
                <div style="font-size: ${fontSize}px;">🐽</div>
                <div style="font-size: ${numberSize}px; font-weight: 700; color: #000; margin-top: -3px;">${count}</div>
               </div>`,
        iconSize: [size, size + 10],
        iconAnchor: [size/2, (size + 10)/2]
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
        
        if (!response.ok) {
            throw new Error(`HTTP error! status: ${response.status}`);
        }
        
        const result = await response.json();
        
        // Ensure result has the expected structure
        if (!result || typeof result !== 'object') {
            console.warn('Invalid viewport data received:', result);
            currentData = { type: 'campgrounds', data: [] };
        } else {
            currentData = result;
        }
        
        renderMarkersFromViewport(currentData);
        updateSaveGroupButton();
    } catch (error) {
        console.error('Failed to load viewport data:', error);
        // Set empty data state on error
        currentData = { type: 'campgrounds', data: [] };
        renderMarkersFromViewport(currentData);
        updateSaveGroupButton();
    }
}

// Event listeners
map.on('moveend zoomend', loadViewportData);

function renderMarkersFromViewport(result) {
    // Clear existing markers
    markers.forEach(marker => map.removeLayer(marker));
    markers = [];
    
    // Handle empty results gracefully
    if (!result || !result.data || result.data.length === 0) {
        console.log('No campgrounds found in current viewport');
        return;
    }
    
    if (result.type === 'clusters') {
        // Render clusters
        result.data.forEach(cluster => {
            const marker = L.marker([cluster.lat, cluster.lon], { 
                icon: createClusterIcon(cluster.count) 
            }).bindPopup(`
                <div class="custom-popup">
                    <div class="popup-title">${cluster.count} Schniffgrounds</div>
                    <div style="margin-top: 0.5rem;">
                        <div style="font-size: 0.8rem; color: #666; margin-top: 0.5rem;">🔍 Zoom in to see the individual schniffgrounds</div>
                    </div>
                </div>
            `).addTo(map);
            
            markers.push(marker);
        });
    } else {
        // Show all campgrounds without filtering
        result.data.forEach(campground => {
            const icon = campground.provider === 'recreation_gov' ? recreationIcon : californiaIcon;
            
            const linkHtml = campground.url ? 
                `<a href="${campground.url}" target="_blank" rel="noopener noreferrer" class="popup-provider ${campground.provider}">🔗 ${campground.provider.replace('_', ' ')}</a>` : 
                `<div class="popup-provider ${campground.provider}">${campground.provider.replace('_', ' ')}</div>`;
            
            const marker = L.marker([campground.lat, campground.lon], { icon })
                .bindPopup(`
                    <div class="custom-popup">
                        <div class="popup-title">${campground.name}</div>
                        ${linkHtml}
                    </div>
                `)
                .addTo(map);
            
            markers.push(marker);
        });
    }
}

// Load initial data
loadViewportData();

function updateStatsFromViewport(result) {
    if (result.type === 'clusters') {
        const totalCount = result.data.reduce((sum, cluster) => sum + cluster.count, 0);
        document.getElementById('stats').textContent = 
            `${totalCount} campgrounds`;
    } else {
        document.getElementById('stats').textContent = 
            `${result.data.length} campgrounds in viewport`;
    }
    
    // Update the save group button
    updateSaveGroupButton();
}

// Event listeners
map.on('moveend zoomend', loadViewportData);

// Place search
let placeSearchTimeout;
document.getElementById('place-search').addEventListener('input', (e) => {
    clearTimeout(placeSearchTimeout);
    placeSearchTimeout = setTimeout(() => {
        if (e.target.value.length > 2) {
            searchPlace(e.target.value);
        } else {
            hideSearchDropdown();
        }
    }, 500); // Timeout for place search to avoid too many API calls
});

// Hide dropdown when clicking outside
document.addEventListener('click', (e) => {
    if (!e.target.closest('.search-container')) {
        hideSearchDropdown();
    }
});

// Also trigger selection of first result on Enter key
document.getElementById('place-search').addEventListener('keypress', (e) => {
    if (e.key === 'Enter') {
        e.preventDefault();
        const firstOption = document.querySelector('.search-dropdown .search-option');
        if (firstOption) {
            firstOption.click();
        }
    }
});

// Load initial data
loadViewportData();

// Save group functionality
function updateSaveGroupButton() {
    const saveGroupBtn = document.getElementById('save-group-btn');
    if (!saveGroupBtn) return;
    
    if (!userToken) {
        saveGroupBtn.style.display = 'none';
        return;
    }
    
    if (!currentData) {
        saveGroupBtn.disabled = true;
        saveGroupBtn.textContent = `👃 No schniffgrounds here`;
        return;
    }
    
    // Since backend now sends individual campgrounds when ≤100, we can check directly
    if (currentData.type === 'clusters') {
        // If we still have clusters, it means >100 campgrounds
        const totalCount = currentData.data ? currentData.data.reduce((sum, cluster) => sum + cluster.count, 0) : 0;
        saveGroupBtn.disabled = true;
        saveGroupBtn.textContent = `� Zoom in (${totalCount} spots found)`;
    } else {
        // We have individual campgrounds (≤100)
        const campgroundCount = currentData.data ? currentData.data.length : 0;
        if (campgroundCount === 0) {
            saveGroupBtn.disabled = true;
            saveGroupBtn.textContent = `� No schniffgrounds here`;
        } else {
            saveGroupBtn.disabled = false;
            saveGroupBtn.textContent = `� Create Schniffgroup (${campgroundCount} spots)`;
        }
    }
}

function openSaveGroupModal() {
    if (!currentData || !userToken) {
        return;
    }
    
    // Only allow if we have individual campgrounds (backend now handles this automatically)
    if (currentData.type === 'clusters') {
        return;
    }
    
    const modal = document.getElementById('save-group-modal');
    const campgroundList = document.getElementById('campground-list');
    
    // Clear existing list
    campgroundList.innerHTML = '';
    
    // Add campgrounds to the list
    currentData.data.forEach(campground => {
        const item = document.createElement('div');
        item.className = 'campground-item';
        item.innerHTML = `
            <label>
                <div style="display: flex; align-items: flex-start; width: 100%; overflow: hidden;">
                    <input type="checkbox" value="${campground.provider}:${campground.id}" data-name="${campground.name}" onchange="updateSaveModalButton()">
                    <div style="flex: 1; min-width: 0; overflow: hidden;">
                        <div class="campground-name">${campground.name}</div>
                        <div class="campground-provider">${campground.provider.replace('_', ' ')}</div>
                    </div>
                </div>
            </label>
        `;
        campgroundList.appendChild(item);
    });
    
    modal.style.display = 'block';
    updateSaveModalButton();
}

function updateSaveModalButton() {
    const saveBtn = document.getElementById('save-modal-btn');
    const nameInput = document.getElementById('group-name');
    const checkedBoxes = document.querySelectorAll('#campground-list input[type="checkbox"]:checked');
    
    const hasName = nameInput.value.trim().length > 0;
    const campgroundCount = checkedBoxes.length;
    
    saveBtn.disabled = !hasName || campgroundCount === 0 || campgroundCount > 10;
    
    if (campgroundCount > 10) {
        saveBtn.textContent = `🚫 Too many! (${campgroundCount}/10)`;
    } else if (campgroundCount === 0) {
        saveBtn.textContent = '📍 Pick your spots';
    } else if (!hasName) {
        saveBtn.textContent = '✍️ Name your schniffgroup';
    } else {
        saveBtn.textContent = `🐽 Create group (${campgroundCount})`;
    }
}

async function saveGroup() {
    const nameInput = document.getElementById('group-name');
    const checkedBoxes = document.querySelectorAll('#campground-list input[type="checkbox"]:checked');
    
    const groupName = nameInput.value.trim();
    const campgrounds = Array.from(checkedBoxes).map(checkbox => {
        const [provider, campgroundId] = checkbox.value.split(':');
        return { provider, campground_id: campgroundId };
    });
    
    try {
        const response = await fetch(`/api/groups/create?user=${encodeURIComponent(userToken)}`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json',
            },
            body: JSON.stringify({
                name: groupName,
                campgrounds: campgrounds
            })
        });
        
        if (!response.ok) {
            const error = await response.text();
            throw new Error(error);
        }
        
        const group = await response.json();
        showSuccessModal(group.name, campgrounds.length);
        closeSaveGroupModal();
    } catch (error) {
        console.error('Failed to save group:', error);
        showErrorModal('Failed to save group: ' + error.message);
    }
}

function showSuccessModal(groupName, campgroundCount) {
    const modal = document.getElementById('success-modal');
    const messageEl = document.getElementById('success-message');

    messageEl.textContent = `${groupName} is ready to schniff`;

    modal.style.display = 'block';
}

function showErrorModal(errorMessage) {
    // For now, fall back to alert for errors - could create an error modal later
    alert('🐽 Oops! Something went sideways: ' + errorMessage);
}

function closeSuccessModal() {
    const modal = document.getElementById('success-modal');
    modal.style.display = 'none';
}

function openDiscordAndClose() {
    const guildId = '1124196592531034173'; // Your Discord server ID
    const botId = '1124195902123413524'; // Schniffomatic9000 bot ID
    
    // Track if the page loses focus (indicating an app opened)
    let appOpened = false;
    let fallbackTimeout;
    
    // Listen for page visibility changes
    const handleVisibilityChange = () => {
        if (document.hidden) {
            // Page lost focus, likely because Discord app opened
            appOpened = true;
            clearTimeout(fallbackTimeout);
            document.removeEventListener('visibilitychange', handleVisibilityChange);
        }
    };
    
    // Listen for page blur (another way to detect app opening)
    const handleBlur = () => {
        appOpened = true;
        clearTimeout(fallbackTimeout);
        window.removeEventListener('blur', handleBlur);
        document.removeEventListener('visibilitychange', handleVisibilityChange);
    };
    
    document.addEventListener('visibilitychange', handleVisibilityChange);
    window.addEventListener('blur', handleBlur);
    
    // Try Discord app first - go to server since DM links are unreliable
    const discordAppUrl = `discord://discord.com/channels/${guildId}`;
    
    // Create a temporary link and try to open the app
    const link = document.createElement('a');
    link.href = discordAppUrl;
    link.style.display = 'none';
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    
    // Set up fallback with longer delay
    fallbackTimeout = setTimeout(() => {
        // Clean up listeners
        document.removeEventListener('visibilitychange', handleVisibilityChange);
        window.removeEventListener('blur', handleBlur);
        
        // Only open web version if app didn't open
        if (!appOpened) {
            // Try web Discord - go to server
            const discordWebUrl = `https://discord.com/channels/${guildId}`;
            window.open(discordWebUrl, '_blank');
        }
    }, 4000); // Longer delay to give user time to respond to prompt
    
    // Close the modal
    closeSuccessModal();
}


function closeSaveGroupModal() {
    const modal = document.getElementById('save-group-modal');
    modal.style.display = 'none';
    
    // Reset form
    document.getElementById('group-name').value = '';
    document.querySelectorAll('#campground-list input[type="checkbox"]').forEach(cb => cb.checked = false);
}

function closeInstructionsModal() {
    const modal = document.getElementById('instructions-modal');
    modal.style.display = 'none';
}
