// Get user token from URL parameters
const urlParams = new URLSearchParams(window.location.search);
const userToken = urlParams.get('user');

// Filter functionality variables
let filterOptions = null;
let currentFilters = {
    amenities: [],
    campsiteTypes: [],
    equipment: [],
    minRating: 0,
    minPrice: 0,
    maxPrice: 500
};

// Initialize the map with mobile-friendly continuous zoom
const map = L.map('map', {
    zoomSnap: 0,      // Allows fractional zoom levels
    zoomDelta: 1,     // Faster zoom steps for PC
    wheelPxPerZoomLevel: 30  // Much faster wheel zoom control for PC
}).setView([39.8283, -98.5795], 4); // Center of US

// Create different tile layers for park-focused viewing
// Cleaner topographic layer with reduced detail at low zoom levels
const topoLayer = L.tileLayer('https://{s}.tile.opentopomap.org/{z}/{x}/{y}.png', {
    attribution: 'Map data: &copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors, <a href="http://viewfinderpanoramas.org">SRTM</a> | Map style: &copy; <a href="https://opentopomap.org">OpenTopoMap</a>'
});

// Enhanced OSM layer that highlights parks and nature areas
const osmLayer = L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', {
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
});

// Add the standard layer as default
osmLayer.addTo(map);

// Store layer references for switching
const mapLayers = {
    standard: osmLayer,
    topographic: topoLayer
};

// Configure popup options globally to remove close button but allow closing on map click
map.options.closePopupOnClick = true;

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
                <div style="font-size: ${numberSize}px; font-weight: 700; color: #000; margin-top: -3px; font-family: 'Syne', sans-serif; letter-spacing: -0.5px;">${count}</div>
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
        zoom: Math.round(zoom),  // Round fractional zoom to integer for API
        // Include filter parameters
        amenities: currentFilters.amenities,
        campsite_types: currentFilters.campsiteTypes,
        equipment: currentFilters.equipment,
        min_rating: currentFilters.minRating,
        min_price: currentFilters.minPrice,
        max_price: currentFilters.maxPrice
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
                    <div class="popup-details">
                        <div style="margin: 0.8rem 0; padding: 0.6rem; background-color: #f0f8f0; border: 2px solid #2d5a3d; border-radius: 4px;">
                            <div style="font-size: 0.9rem; color: #2d5a3d; font-weight: 600;">🔍 Zoom in to explore individual schniffgrounds</div>
                        </div>
                    </div>
                </div>
            `, {
                closeButton: false,
                maxWidth: 300,
                className: 'narrow-popup',
                autoPan: false
            }).addTo(map);
            
            markers.push(marker);
        });
    } else {
        // Show all campgrounds (filtering now done on server)
        result.data.forEach(campground => {
            const icon = campground.provider === 'recreation_gov' ? recreationIcon : californiaIcon;
            
            // Create enhanced popup with park-relevant information
            const providerName = campground.provider === 'recreation_gov' ? 'Recreation.gov' : 'Reserve California';
            const providerEmoji = campground.provider === 'recreation_gov' ? '🏞️' : '🌲';
            
            // Format rating display with outlined stars and bordered style
            const ratingDisplay = campground.rating > 0 
                ? `<div class="popup-rating">⭐ ${campground.rating.toFixed(1)}/5.0</div>`
                : '';
            
            // Format price display
            let priceDisplay = '';
            if (campground.price_min > 0 || campground.price_max > 0) {
                const unit = campground.price_unit || 'night';
                if (campground.price_min === campground.price_max) {
                    priceDisplay = `<div class="popup-price">💰 $${campground.price_min.toFixed(0)}/${unit}</div>`;
                } else if (campground.price_min > 0 && campground.price_max > 0) {
                    priceDisplay = `<div class="popup-price">💰 $${campground.price_min.toFixed(0)}-$${campground.price_max.toFixed(0)}/${unit}</div>`;
                } else if (campground.price_max > 0) {
                    priceDisplay = `<div class="popup-price">💰 Up to $${campground.price_max.toFixed(0)}/${unit}</div>`;
                } else {
                    priceDisplay = `<div class="popup-price">💰 From $${campground.price_min.toFixed(0)}/${unit}</div>`;
                }
            }
            
            // Display price and rating on separate lines
            let priceRatingDisplay = '';
            if (ratingDisplay || priceDisplay) {
                priceRatingDisplay = `${ratingDisplay}${priceDisplay}`;
            }
            
            // Format campsite types display
            let campsiteTypesDisplay = '';
            if (campground.campsite_types && campground.campsite_types.length > 0) {
                const types = campground.campsite_types.slice(0, 3).join(', ');
                campsiteTypesDisplay = `<div class="popup-campsite-types">📍 ${types}</div>`;
            }
            
            // Format equipment display
            let equipmentDisplay = '';
            if (campground.equipment && campground.equipment.length > 0) {
                const equipment = campground.equipment.slice(0, 3).join(', ');
                equipmentDisplay = `<div class="popup-equipment">🛖 ${equipment}</div>`;
            }
            
            // Format image display
            let imageDisplay = '';
            if (campground.image_url) {
                imageDisplay = `<div class="popup-image">
                    <img src="${campground.image_url}" alt="${campground.name}" loading="lazy" />
                </div>`;
            }
            
            // Format amenities display (show all amenities)
            let amenitiesDisplay = '';
            if (campground.amenities && campground.amenities.length > 0) {
                const amenityList = campground.amenities.map(name => `• ${name}`).join('<br>');
                amenitiesDisplay = `<div class="popup-amenities">
                    <strong>🛝 Features</strong><br>
                    ${amenityList}
                </div>`;
            }
            
            const linkHtml = campground.url ? 
                `<div class="popup-provider-line">
                    <a href="${campground.url}" target="_blank" rel="noopener noreferrer" class="popup-provider ${campground.provider}">
                        ${providerName} →
                    </a>
                    ${priceRatingDisplay}
                </div>` : 
                `<div class="popup-provider-line">
                    <div class="popup-provider ${campground.provider}">
                        ${providerName}
                    </div>
                    ${priceRatingDisplay}
                </div>`;
            
            const marker = L.marker([campground.lat, campground.lon], { icon })
                .bindPopup(`
                    <div class="custom-popup">
                        ${imageDisplay}
                        <div class="popup-title">${campground.name}</div>
                        <div class="popup-details">
                            ${campsiteTypesDisplay}
                            ${equipmentDisplay}
                            ${amenitiesDisplay}
                        </div>
                        ${linkHtml}
                        <div class="popup-actions">
                            <button onclick="getDirections(event, ${campground.lat}, ${campground.lon})" class="map-action-btn">
                                🧭 Directions
                            </button>
                        </div>
                    </div>
                `, {
                    closeButton: false,
                    maxWidth: 350,
                    className: 'narrow-popup',
                    autoPan: false
                })
                .addTo(map);
            
            markers.push(marker);
        });
    }
}

// Load initial data
loadViewportData();

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
        saveGroupBtn.textContent = `🔍 Too many schniffgrounds (${totalCount})`;
    } else {
        // We have individual campgrounds (≤100)
        const campgroundCount = currentData.data ? currentData.data.length : 0;
        if (campgroundCount === 0) {
            saveGroupBtn.disabled = true;
            saveGroupBtn.textContent = `🚫 No schniffgrounds here`;
        } else {
            saveGroupBtn.disabled = false;
            saveGroupBtn.textContent = `🚀 Refine Schniffgroup (${campgroundCount})`;
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
        
        // Format rating display for modal
        const ratingDisplay = campground.rating > 0 
            ? `<span class="campground-rating">⭐ ${campground.rating.toFixed(2)}</span>`
            : '';
            
        // Format price display for modal
        let priceDisplay = '';
        if (campground.price_min > 0 || campground.price_max > 0) {
            const unit = campground.price_unit || 'night';
            if (campground.price_min === campground.price_max) {
                priceDisplay = `<span class="campground-price">$${campground.price_min.toFixed(0)}/${unit}</span>`;
            } else if (campground.price_min > 0 && campground.price_max > 0) {
                priceDisplay = `<span class="campground-price">$${campground.price_min.toFixed(0)}-$${campground.price_max.toFixed(0)}/${unit}</span>`;
            } else if (campground.price_max > 0) {
                priceDisplay = `<span class="campground-price">Up to $${campground.price_max.toFixed(0)}/${unit}</span>`;
            }
        }
            
        // Format amenities for modal (show first 3 key amenities)
        let amenitiesDisplay = '';
        if (campground.amenities && campground.amenities.length > 0) {
            const topAmenities = campground.amenities
                .filter(name => !name.startsWith('Equipment:') && !name.includes('Description'))
                .join(', ');
            if (topAmenities) {
                amenitiesDisplay = `<div class="campground-amenities">🛝${topAmenities}</div>`;
            }
        }
        
        item.innerHTML = `
            <label>
                <div style="display: flex; align-items: flex-start; width: 100%; overflow: hidden;">
                    <input type="checkbox" value="${campground.provider}:${campground.id}" data-name="${campground.name}" onchange="updateSaveModalButton()">
                    <div style="flex: 1; min-width: 0; overflow: hidden;">
                        <div class="campground-name">
                            ${campground.name}
                        </div>
                        <div class="campground-provider-line">
                            <span class="campground-provider">${campground.provider.replace('_', ' ')}</span>
                            ${ratingDisplay}
                            ${priceDisplay}
                        </div>
                        ${amenitiesDisplay}
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

// Map utility functions for enhanced park features
function showOnMap(event, lat, lon) {
    // Prevent event bubbling to avoid closing popup
    event.stopPropagation();
    map.setView([lat, lon], Math.max(map.getZoom(), 14));
}

function getDirections(event, lat, lon) {
    // Prevent event bubbling to avoid closing popup
    event.stopPropagation();
    
    // Use Google Maps for all platforms - it works reliably everywhere
    // On mobile, Google Maps web will offer to open the native app
    const url = `https://www.google.com/maps/dir/?api=1&destination=${lat},${lon}`;
    window.open(url, '_blank');
}

// Filter functionality
async function loadFilterOptions() {
    try {
        const response = await fetch('/api/filter-options');
        if (!response.ok) {
            throw new Error('Failed to fetch filter options');
        }
        filterOptions = await response.json();
        
        // Update slider ranges based on actual data
        if (filterOptions.price_range) {
            const priceMinSlider = document.getElementById('price-min-slider');
            const priceMaxSlider = document.getElementById('price-max-slider');
            priceMinSlider.min = Math.floor(filterOptions.price_range.min);
            priceMinSlider.max = Math.ceil(filterOptions.price_range.max);
            priceMinSlider.value = Math.floor(filterOptions.price_range.min);
            priceMaxSlider.min = Math.floor(filterOptions.price_range.min);
            priceMaxSlider.max = Math.ceil(filterOptions.price_range.max);
            priceMaxSlider.value = Math.ceil(filterOptions.price_range.max);
            
            currentFilters.minPrice = Math.floor(filterOptions.price_range.min);
            currentFilters.maxPrice = Math.ceil(filterOptions.price_range.max);
            
            updatePriceMinValue(currentFilters.minPrice);
            updatePriceMaxValue(currentFilters.maxPrice);
        }
        
        if (filterOptions.rating_range) {
            const ratingSlider = document.getElementById('rating-slider');
            ratingSlider.min = filterOptions.rating_range.min;
            ratingSlider.max = filterOptions.rating_range.max;
            ratingSlider.value = filterOptions.rating_range.min;
            currentFilters.minRating = filterOptions.rating_range.min;
            updateRatingValue(currentFilters.minRating);
        }
        
        populateFilterOptions();
    } catch (error) {
        console.error('Failed to load filter options:', error);
    }
}

function populateFilterOptions() {
    if (!filterOptions) return;
    
    // Populate amenities
    const amenitiesContainer = document.getElementById('amenities-container');
    amenitiesContainer.innerHTML = '';
    if (filterOptions.amenities) {
        filterOptions.amenities.sort().forEach(amenity => {
            const item = createFilterCheckbox('amenity', amenity, amenity);
            amenitiesContainer.appendChild(item);
        });
    }
    
    // Populate campsite types
    const campsiteTypesContainer = document.getElementById('campsite-types-container');
    campsiteTypesContainer.innerHTML = '';
    if (filterOptions.campsite_types) {
        filterOptions.campsite_types.sort().forEach(type => {
            const item = createFilterCheckbox('campsite-type', type, type);
            campsiteTypesContainer.appendChild(item);
        });
    }
    
    // Populate equipment
    const equipmentContainer = document.getElementById('equipment-container');
    equipmentContainer.innerHTML = '';
    if (filterOptions.equipment) {
        filterOptions.equipment.sort().forEach(equipment => {
            const item = createFilterCheckbox('equipment', equipment, equipment);
            equipmentContainer.appendChild(item);
        });
    }
}

function createFilterCheckbox(type, value, label) {
    const item = document.createElement('div');
    item.className = 'filter-checkbox-item';
    
    const checkbox = document.createElement('input');
    checkbox.type = 'checkbox';
    checkbox.id = `${type}-${value.replace(/\s+/g, '-').toLowerCase()}`;
    checkbox.value = value;
    checkbox.addEventListener('change', updateFiltersFromCheckboxes);
    
    const labelEl = document.createElement('label');
    labelEl.htmlFor = checkbox.id;
    labelEl.textContent = label;
    
    item.appendChild(checkbox);
    item.appendChild(labelEl);
    
    return item;
}

function updateFiltersFromCheckboxes() {
    // Update amenities
    const amenityCheckboxes = document.querySelectorAll('#amenities-container input[type="checkbox"]:checked');
    currentFilters.amenities = Array.from(amenityCheckboxes).map(cb => cb.value);
    
    // Update campsite types
    const campsiteTypeCheckboxes = document.querySelectorAll('#campsite-types-container input[type="checkbox"]:checked');
    currentFilters.campsiteTypes = Array.from(campsiteTypeCheckboxes).map(cb => cb.value);
    
    // Update equipment
    const equipmentCheckboxes = document.querySelectorAll('#equipment-container input[type="checkbox"]:checked');
    currentFilters.equipment = Array.from(equipmentCheckboxes).map(cb => cb.value);
}

function updateRatingValue(value) {
    currentFilters.minRating = parseFloat(value);
    document.getElementById('rating-value').textContent = parseFloat(value).toFixed(1);
}

function updatePriceMinValue(value) {
    currentFilters.minPrice = parseInt(value);
    document.getElementById('price-min-value').textContent = value;
    
    // Ensure min doesn't exceed max
    const maxSlider = document.getElementById('price-max-slider');
    if (parseInt(value) > currentFilters.maxPrice) {
        maxSlider.value = value;
        updatePriceMaxValue(value);
    }
}

function updatePriceMaxValue(value) {
    currentFilters.maxPrice = parseInt(value);
    document.getElementById('price-max-value').textContent = value;
    
    // Ensure max doesn't go below min
    const minSlider = document.getElementById('price-min-slider');
    if (parseInt(value) < currentFilters.minPrice) {
        minSlider.value = value;
        updatePriceMinValue(value);
    }
}

function openFilterModal() {
    if (!filterOptions) {
        loadFilterOptions();
    }
    document.getElementById('filter-modal').style.display = 'block';
}

function closeFilterModal() {
    document.getElementById('filter-modal').style.display = 'none';
}

function clearAllFilters() {
    // Reset checkboxes
    document.querySelectorAll('#amenities-container input[type="checkbox"]').forEach(cb => cb.checked = false);
    document.querySelectorAll('#campsite-types-container input[type="checkbox"]').forEach(cb => cb.checked = false);
    document.querySelectorAll('#equipment-container input[type="checkbox"]').forEach(cb => cb.checked = false);
    
    // Reset sliders
    const ratingSlider = document.getElementById('rating-slider');
    ratingSlider.value = filterOptions?.rating_range?.min || 0;
    updateRatingValue(ratingSlider.value);
    
    const priceMinSlider = document.getElementById('price-min-slider');
    const priceMaxSlider = document.getElementById('price-max-slider');
    priceMinSlider.value = filterOptions?.price_range?.min || 0;
    priceMaxSlider.value = filterOptions?.price_range?.max || 500;
    updatePriceMinValue(priceMinSlider.value);
    updatePriceMaxValue(priceMaxSlider.value);
    
    // Reset current filters
    currentFilters = {
        amenities: [],
        campsiteTypes: [],
        equipment: [],
        minRating: filterOptions?.rating_range?.min || 0,
        minPrice: filterOptions?.price_range?.min || 0,
        maxPrice: filterOptions?.price_range?.max || 500
    };
}

function applyFilters() {
    updateFiltersFromCheckboxes();
    closeFilterModal();
    loadViewportData(); // Reload data with filters
}

function matchesFilters(campground) {
    // Check rating filter
    if (campground.rating < currentFilters.minRating) {
        return false;
    }
    
    // Check price filter
    if (campground.price_min > 0 && campground.price_min < currentFilters.minPrice) {
        return false;
    }
    if (campground.price_max > 0 && campground.price_max > currentFilters.maxPrice) {
        return false;
    }
    
    // Check amenities filter
    if (currentFilters.amenities.length > 0) {
        const hasRequiredAmenity = currentFilters.amenities.some(amenity => 
            campground.amenities && campground.amenities.includes(amenity)
        );
        if (!hasRequiredAmenity) {
            return false;
        }
    }
    
    // Check campsite types filter
    if (currentFilters.campsiteTypes.length > 0) {
        const hasRequiredType = currentFilters.campsiteTypes.some(type => 
            campground.campsite_types && campground.campsite_types.includes(type)
        );
        if (!hasRequiredType) {
            return false;
        }
    }
    
    // Check equipment filter
    if (currentFilters.equipment.length > 0) {
        const hasRequiredEquipment = currentFilters.equipment.some(equipment => 
            campground.equipment && campground.equipment.includes(equipment)
        );
        if (!hasRequiredEquipment) {
            return false;
        }
    }
    
    return true;
}

// Map layer switching
function switchMapLayer(layerType) {
    // Remove all layers first
    Object.values(mapLayers).forEach(layer => {
        if (map.hasLayer(layer)) {
            map.removeLayer(layer);
        }
    });
    
    // Add the selected layer
    if (mapLayers[layerType]) {
        mapLayers[layerType].addTo(map);
    }
}

// Load filter options on page load
document.addEventListener('DOMContentLoaded', loadFilterOptions);
