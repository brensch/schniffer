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
        updateSaveGroupButton();
    } catch (error) {
        console.error('Failed to load viewport data:', error);
        updateSaveGroupButton();
    }
}

// Event listeners
map.on('moveend zoomend', loadViewportData);

function renderMarkersFromViewport(result) {
    // Clear existing markers
    markers.forEach(marker => map.removeLayer(marker));
    markers = [];
    
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
        // Show all campgrounds without filtering
        result.data.forEach(campground => {
            const icon = campground.provider === 'recreation_gov' ? recreationIcon : californiaIcon;
            
            const linkHtml = campground.url ? 
                `<div class="popup-link"><a href="${campground.url}" target="_blank" rel="noopener noreferrer">View on ${campground.provider.replace('_', ' ')}</a></div>` : '';
            
            const marker = L.marker([campground.lat, campground.lon], { icon })
                .bindPopup(`
                    <div class="custom-popup">
                        <div class="popup-title">${campground.name}</div>
                        <div class="popup-provider ${campground.provider}">${campground.provider.replace('_', ' ')}</div>
                        <div class="popup-coordinates">
                            ${campground.lat.toFixed(4)}, ${campground.lon.toFixed(4)}
                        </div>
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
    
    if (!currentData || currentData.type === 'clusters') {
        const totalCount = currentData ? currentData.data.reduce((sum, cluster) => sum + cluster.count, 0) : 0;
        saveGroupBtn.disabled = true;
        saveGroupBtn.textContent = `Zoom in to save group (${totalCount} campgrounds)`;
        return;
    }
    
    const campgroundCount = currentData.data.length;
    
    if (campgroundCount > 100) {
        saveGroupBtn.disabled = true;
        saveGroupBtn.textContent = `Too many for a group (${campgroundCount} campgrounds)`;
    } else {
        saveGroupBtn.disabled = false;
        saveGroupBtn.textContent = `Save Group (${campgroundCount} campgrounds)`;
    }
}

function openSaveGroupModal() {
    if (!currentData || currentData.type === 'clusters' || !userToken) {
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
                <input type="checkbox" value="${campground.provider}:${campground.id}" data-name="${campground.name}" onchange="updateSaveModalButton()">
                <span class="campground-name">${campground.name}</span>
                <span class="campground-provider">${campground.provider.replace('_', ' ')}</span>
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
        saveBtn.textContent = `Too many selected (${campgroundCount}/10)`;
    } else if (campgroundCount === 0) {
        saveBtn.textContent = 'Select campgrounds';
    } else if (!hasName) {
        saveBtn.textContent = 'Enter group name';
    } else {
        saveBtn.textContent = `Save Group (${campgroundCount})`;
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
    
    const funkyMessages = [
        `🐽 For fame to the eye of heaven, "${groupName}" is anointed with the oils of schniffing! 🐽`,
        `🎉 The humble consequence of carbon turned diamond by the sun - "${groupName}" burns bright with the desire to schniff! 🎉`,
        `💫 "${groupName}" is the vortex at the center of the vortex, selected for greatness by the finger of schniffing power! 💫`,
        `🐽 "${groupName}" operates from a platform of power and has zero respect for indecision! 🐽`,
        `✨ "${groupName}" is the shining arc of schniffing, ready to curse and spit their name at the heavens! ✨`,
        `🎪 "${groupName}" has traveled this nation and learned the common denominator is American schniffing exceptionalism! 🎪`,
        `🐽 "${groupName}" is the eighth archangel of schniffing, seven feet from tip of wing to tip of wing! 🐽`
    ];
    
    const randomMessage = funkyMessages[Math.floor(Math.random() * funkyMessages.length)];
    messageEl.textContent = `${randomMessage} With ${campgroundCount} epic schniffgrounds ready to explore!`;
    
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
    
    // Try to open Discord app
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
