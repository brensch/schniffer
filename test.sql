SELECT
  cg.provider,
  cg.campground_id,
  cg.name,
  COUNT(DISTINCT ca.campsite_id) AS available_campsites,
  GROUP_CONCAT(DISTINCT cm.campsite_type) AS campsite_types,
  GROUP_CONCAT(DISTINCT ce.equipment_type) AS equipment_types
FROM campgrounds cg
JOIN campsite_availability ca
  ON cg.provider = ca.provider AND cg.campground_id = ca.campground_id
JOIN campsite_metadata cm
  ON ca.provider = cm.provider AND ca.campground_id = cm.campground_id AND ca.campsite_id = cm.campsite_id
JOIN campsite_equipment ce
  ON ca.provider = ce.provider AND ca.campground_id = ce.campground_id AND ca.campsite_id = ce.campsite_id
WHERE
  cg.latitude BETWEEN 9.79320814556633 AND 66.48781027943105
  AND cg.longitude BETWEEN -139.6947189157936 AND -64.24048268565882
  AND ca.available = 1
  AND cm.campsite_type IN (
    'ada campsite','anchorage','ball field','bike in campsite','boat in','boat in campsite','boat in primitive campsite','cabin (3 people)','cabin (4 people)','cabin (6 people)','cabin (8 people)','cabin electric','cabin nonelectric','campsite','campsite (small)','deluxe campsite','equestrian campsite','equestrian electric','equestrian group campsite','equestrian group primitive campsite','equestrian group tent campsite','equestrian nonelectric','equestrian primitive campsite','equestrian tent campsite','group camping','group campsite','group dailyuse (a)','group dailyuse (b)','group dailyuse (c)','group day use','group equestrian','group hike to','group picnic area','group primitive campsite','group rv area nonelectric','group shelter electric','group shelter nonelectric','group standard area electric','group standard area nonelectric','group standard electric','group standard nonelectric','group tent campsite','group tent only area nonelectric','group tent primitive campsite','group walk to','hike in campsite','hike in primitive campsite','hike to','hike/bike campsite','hook up (e ) campsite','hook up (e/w) campsite','hook up (e/w/g) campsite','hook up (e/w/s) campsite','hook up campsite','lookout','management','mini-group campsite','mooring','off road vehicle campsite','overnight shelter electric','overnight shelter nonelectric','parking','picnic','premium boat in primitive campsite','premium cabin (8 people)','premium campsite','premium campsite (small)','premium double hook up (e/w/s) campsite','premium group campsite','premium hook up (e ) campsite','premium hook up (e/w) campsite','premium hook up (e/w/s) campsite','premium tent campsite','premium tent campsite (small)','primitive campsite','rv electric','rv nonelectric','shelter electric','shelter nonelectric','standard electric','standard nonelectric','tent campsite','tent campsite (small)','tent hike in primitive campsite','tent only - walk-in','tent only electric','tent only nonelectric','tent primitive campsite','walk to','yurt','zone'
  )
  AND ce.equipment_type IN (
    'boat','car','caravan/camper van','fifth wheel','hammock','horse','large tent over 9x12`','pickup camper','pop up','rv','rv up to 1 ft','rv up to 10 ft','rv up to 100 ft','rv up to 11 ft','rv up to 12 ft','rv up to 13 ft','rv up to 14 ft','rv up to 15 ft','rv up to 16 ft','rv up to 17 ft','rv up to 18 ft','rv up to 19 ft','rv up to 20 ft','rv up to 21 ft','rv up to 22 ft','rv up to 23 ft','rv up to 24 ft','rv up to 25 ft','rv up to 26 ft','rv up to 27 ft','rv up to 28 ft','rv up to 29 ft','rv up to 30 ft','rv up to 31 ft','rv up to 32 ft','rv up to 33 ft','rv up to 34 ft','rv up to 35 ft','rv up to 36 ft','rv up to 39 ft','rv up to 40 ft','rv up to 41 ft','rv up to 42 ft','rv up to 45 ft','rv up to 49 ft','rv up to 50 ft','rv up to 55 ft','rv up to 57 ft','rv up to 59 ft','rv up to 60 ft','rv up to 65 ft','rv up to 70 ft','rv up to 75 ft','rv/motorhome','small tent','standard','tent','trailer','vehicle'
  )
  AND cm.rating >= 0
  AND cm.cost_per_night BETWEEN 5 AND 650
GROUP BY cg.provider, cg.campground_id
HAVING COUNT(DISTINCT ca.campsite_id) > 0;
