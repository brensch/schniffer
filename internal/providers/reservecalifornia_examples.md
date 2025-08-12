
https://calirdr.usedirect.com/RDR/rdr/fd/citypark

this returns a payload like this with all the parks in it

{
    "1": {
        "CityParkId": 1,
        "Name": "Hearst Castle",
        "Latitude": 35.58361111,
        "Longitude": -121.1205556,
        "IsActive": true,
        "EntityType": "Park",
        "EnterpriseId": 1,
        "ParkSize": "Medium\r\n",
        "PlaceId": 1
    },
    "2": {
        "CityParkId": 2,
        "Name": "Anza-Borrego Desert SP",
        "Latitude": 33.25690486,
        "Longitude": -116.4060118,
        "IsActive": true,
        "EntityType": "Park",
        "EnterpriseId": 1,
        "ParkSize": "Medium",
        "PlaceId": 2
    },
}


if 2 is the city park id then you can look up all the places under it like this:

curl 'https://calirdr.usedirect.com/RDR/rdr/search/place' \
  -H 'accept: application/json' \
  -H 'content-type: application/json' \
  -H 'origin: https://reservecalifornia.com' \
  -H 'referer: https://reservecalifornia.com/' \
  --data-raw '{
    "PlaceId": "2"
  }'

which returns 

  {
    "Message": "Built in 6.4166 ms size 17226 bytes on IP-AC1226BC",
    "SelectedPlaceId": 713,
    "HighlightedPlaceId": 0,
    "Latitude": 0.0,
    "Longitude": 0.0,
    "StartDate": "2025-08-12",
    "EndDate": "2025-08-12",
    "NightsRequested": 1,
    "NightsActual": 1,
    "CountNearby": true,
    "NearbyLimit": 50,
    "Sort": "distance",
    "CustomerId": null,
    "Filters": null,
    "AvailablePlaces": 0,
    "SelectedPlace": {
        "PlaceId": 713,
        "Name": "Hearst San Simeon SP",
        "Description": "Enjoy the scenic beauty of the Pacific Ocean while camping at San Simeon Creek Campground (water + showers) or Washburn Campground (water fill-up in lower campground + no showers).  This tranquil getaway has many recreational options.   Beach is closed to dogs for the protection of the threatened Snowey Plover shorebird.  ",
        "HasAlerts": true,
        "IsFavourite": false,
        "Allhighlights": "Birdwatching<br>Boating<br>Boat launch<br>Body surfing<br>Camping<br>Fishing<br>Hiking<br>Picnic area<br>Scuba diving<br>Surfing<br>Swimming<br>",
        "Url": "http://www.parks.ca.gov/?page_id=590",
        "ImageUrl": "https://cali-content.usedirect.com/Images/California/ParkImages/Place/713.jpg",
        "BannerUrl": "https://cali-content.usedirect.com/Images/California/ParkImages/banner.jpg",
        "ParkSize": "Small",
        "Latitude": 35.59939284,
        "Longitude": -121.1254548,
        "TimeZone": "Pacific Standard Time",
        "TimeStamp": "2025-08-12 01:23:33",
        "MilesFromSelected": 0,
        "Available": false,
        "AvailableFiltered": false,
        "ParkCategoryId": 1,
        "ParkActivity": 1,
        "ParkPopularity": 0,
        "AvailableUnitCount": 0,
        "Restrictions": {
            "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
            "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
            "MinimumStay": 1,
            "MaximumStay": 10,
            "IsRestrictionValid": true,
            "Time": "0001-01-01T00:00:00"
        },
        "Facilities": {
            "1": {
                "FacilityId": 789,
                "Name": "Creek Campground Upper Section (sites 1-35)",
                "Description": "Creek Campground Upper Section (sites 1-35)",
                "RateMessage": null,
                "FacilityType": 2,
                "FacilityTypeNew": 1,
                "InSeason": false,
                "Available": false,
                "AvailableFiltered": false,
                "Restrictions": {
                    "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
                    "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
                    "MinimumStay": 1,
                    "MaximumStay": 30,
                    "IsRestrictionValid": true,
                    "Time": "0001-01-01T00:00:00"
                },
                "Latitude": 35.599058,
                "Longitude": -121.122464,
                "Category": "Campgrounds",
                "EnableCheckOccupancy": false,
                "AvailableOccupancy": null,
                "FacilityAllowWebBooking": true,
                "UnitTypes": {
                    "4303": {
                        "UnitTypeId": 4303,
                        "UseType": 4,
                        "Name": "Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 1,
                        "MaxVehicleLength": 35,
                        "HasAda": false,
                        "Restrictions": null,
                        "AvailableCount": 0
                    },
                    "4328": {
                        "UnitTypeId": 4328,
                        "UseType": 4,
                        "Name": "Tent Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 5,
                        "MaxVehicleLength": 35,
                        "HasAda": false,
                        "Restrictions": null,
                        "AvailableCount": 0
                    }
                },
                "IsAvailableForGroup": false,
                "IsAvailableForPatron": false,
                "IsAvailableForEducationalGorup": false,
                "IsAvailableForCto": false,
                "FacilityBehaviourType": 0
            },
            "2": {
                "FacilityId": 687,
                "Name": "Creek Campground Lower Section (sites 36-115)",
                "Description": "Creek Campground Lower Section (sites 36-134)",
                "RateMessage": null,
                "FacilityType": 2,
                "FacilityTypeNew": 1,
                "InSeason": false,
                "Available": false,
                "AvailableFiltered": false,
                "Restrictions": {
                    "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
                    "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
                    "MinimumStay": 1,
                    "MaximumStay": 30,
                    "IsRestrictionValid": true,
                    "Time": "0001-01-01T00:00:00"
                },
                "Latitude": 35.59713,
                "Longitude": -121.124702,
                "Category": "Campgrounds",
                "EnableCheckOccupancy": false,
                "AvailableOccupancy": null,
                "FacilityAllowWebBooking": true,
                "UnitTypes": {
                    "4328": {
                        "UnitTypeId": 4328,
                        "UseType": 4,
                        "Name": "Tent Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 5,
                        "MaxVehicleLength": 35,
                        "HasAda": false,
                        "Restrictions": null,
                        "AvailableCount": 0
                    },
                    "4303": {
                        "UnitTypeId": 4303,
                        "UseType": 4,
                        "Name": "Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 1,
                        "MaxVehicleLength": 35,
                        "HasAda": true,
                        "Restrictions": null,
                        "AvailableCount": 0
                    }
                },
                "IsAvailableForGroup": false,
                "IsAvailableForPatron": false,
                "IsAvailableForEducationalGorup": false,
                "IsAvailableForCto": false,
                "FacilityBehaviourType": 0
            },
            "3": {
                "FacilityId": 788,
                "Name": "Creek Tent Campground (sites 116-134)",
                "Description": "Creek Tent Campground (sites 116-131)",
                "RateMessage": null,
                "FacilityType": 2,
                "FacilityTypeNew": 1,
                "InSeason": false,
                "Available": false,
                "AvailableFiltered": false,
                "Restrictions": {
                    "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
                    "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
                    "MinimumStay": 1,
                    "MaximumStay": 30,
                    "IsRestrictionValid": true,
                    "Time": "0001-01-01T00:00:00"
                },
                "Latitude": 35.596581,
                "Longitude": -121.125556,
                "Category": "Campgrounds",
                "EnableCheckOccupancy": false,
                "AvailableOccupancy": null,
                "FacilityAllowWebBooking": true,
                "UnitTypes": {
                    "4318": {
                        "UnitTypeId": 4318,
                        "UseType": 4,
                        "Name": "Hike In Primitive Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1014,
                        "UnitTypeGroupId": 11,
                        "MaxVehicleLength": 35,
                        "HasAda": false,
                        "Restrictions": null,
                        "AvailableCount": 0
                    },
                    "4328": {
                        "UnitTypeId": 4328,
                        "UseType": 4,
                        "Name": "Tent Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 5,
                        "MaxVehicleLength": 35,
                        "HasAda": false,
                        "Restrictions": null,
                        "AvailableCount": 0
                    }
                },
                "IsAvailableForGroup": false,
                "IsAvailableForPatron": false,
                "IsAvailableForEducationalGorup": false,
                "IsAvailableForCto": false,
                "FacilityBehaviourType": 0
            },
            "4": {
                "FacilityId": 787,
                "Name": "Washburn Campground (sites 201-268)",
                "Description": "Washburn Campground (sites 201-268)",
                "RateMessage": null,
                "FacilityType": 2,
                "FacilityTypeNew": 1,
                "InSeason": false,
                "Available": false,
                "AvailableFiltered": false,
                "Restrictions": {
                    "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
                    "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
                    "MinimumStay": 1,
                    "MaximumStay": 30,
                    "IsRestrictionValid": true,
                    "Time": "0001-01-01T00:00:00"
                },
                "Latitude": 35.59486,
                "Longitude": -121.11112,
                "Category": "Campgrounds",
                "EnableCheckOccupancy": false,
                "AvailableOccupancy": null,
                "FacilityAllowWebBooking": true,
                "UnitTypes": {
                    "4327": {
                        "UnitTypeId": 4327,
                        "UseType": 4,
                        "Name": "Primitive Campsite",
                        "Available": false,
                        "AvailableFiltered": false,
                        "UnitCategoryId": 1,
                        "UnitTypeGroupId": 1,
                        "MaxVehicleLength": 35,
                        "HasAda": true,
                        "Restrictions": null,
                        "AvailableCount": 0
                    }
                },
                "IsAvailableForGroup": false,
                "IsAvailableForPatron": false,
                "IsAvailableForEducationalGorup": false,
                "IsAvailableForCto": false,
                "FacilityBehaviourType": 0
            }
        },
        "IsAvailableForGreatwalk": false,
        "FacilityDefaultZoom": 0,
        "IsReservationAnyDrawActive": true,
        "IsReservationDrawActive": false
    },
    "NearbyPlaces": [
        {
            "PlaceId": 681,
            "Name": "Morro Strand SB",
            "Description": "Morro Strand State Beach is a three-mile coastal frontage park. From the beach, visitors can view the entire Estero Bay, including Morro Rock. Activities include fishing, surfing, wind and kite surfing, kite flying, sunbathing, strolling on the beach, bird watching, and exploring the town of Morro Bay. ",
            "HasAlerts": true,
            "IsFavourite": false,
            "Allhighlights": "Boating<br>Body surfing<br>Camping<br>Fishing<br>Horseback riding<br>Picnic area<br>Rinse Showers<br>Surfing<br>Swimming<br>",
            "Url": "http://www.parks.ca.gov/?page_id=593",
            "ImageUrl": "https://cali-content.usedirect.com/Images/California/ParkImages/Place/681.jpg",
            "BannerUrl": null,
            "ParkSize": "Medium",
            "Latitude": 35.40199484,
            "Longitude": -120.8678627,
            "TimeZone": "Pacific Standard Time",
            "TimeStamp": "2025-08-12 01:23:33",
            "MilesFromSelected": 20,
            "Available": false,
            "AvailableFiltered": false,
            "ParkCategoryId": 2,
            "ParkActivity": 1,
            "ParkPopularity": 0,
            "AvailableUnitCount": 0,
            "Restrictions": {
                "FutureBookingStarts": "2025-08-14T00:00:00-07:00",
                "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
                "MinimumStay": 1,
                "MaximumStay": 10,
                "IsRestrictionValid": false,
                "Time": "0001-01-01T00:00:00"
            },
            "Facilities": {},
            "IsAvailableForGreatwalk": false,
            "FacilityDefaultZoom": 0,
            "IsReservationAnyDrawActive": true,
            "IsReservationDrawActive": false
        },
        
    ]
}

then to get the availability the commands are here. note this has a way to specify the time over which to check.

brensch@Schlaptop:~$ curl 'https://calirdr.usedirect.com/RDR/rdr/search/grid' \
  -H 'accept: application/json' \
  -H 'accept-language: en-US,en-AU;q=0.9,en;q=0.8' \
  -H 'content-type: application/json' \
  -H 'origin: https://reservecalifornia.com' \
  -H 'priority: u=1, i' \
  -H 'referer: https://reservecalifornia.com/' \
  -H 'sec-ch-ua: "Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"' \
  -H 'sec-ch-ua-mobile: ?0' \
  -H 'sec-ch-ua-platform: "Windows"' \
  -H 'sec-fetch-dest: empty' \
  -H 'sec-fetch-mode: cors' \
  -H 'sec-fetch-site: cross-site' \
  -H 'user-agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36' \
  --data-raw '{"IsADA":false,"MinVehicleLength":0,"UnitCategoryId":0,"StartDate":"2025-08-12","WebOnly":true,"UnitTypesGroupIds":[],"SleepingUnitId":0,"EndDate":"2025-08-19","UnitSort":"orderby","InSeasonOnly":true,"FacilityId":"687","RestrictADA":false}'

{
    "Message": "Built in 20.0198 ms size 149959 bytes on IP-AC1226BC",
    "Filters": {
        "InSeasonOnly": "True",
        "WebOnly": "True",
        "IsADA": "False",
        "SleepingUnitId": "0",
        "MinVehicleLength": "0",
        "UnitCategoryId": "0"
    },
    "UnitTypeId": 0,
    "StartDate": "2025-08-12",
    "EndDate": "2025-08-19",
    "NightsRequested": 8,
    "NightsActual": 8,
    "TodayDate": "2025-08-12",
    "TimeZone": "Pacific Standard Time",
    "TimeStamp": "2025-08-12 01:27:48",
    "MinDate": "2025-08-12",
    "MaxDate": "2026-02-11",
    "AvailableUnitsOnly": false,
    "UnitSort": "orderby",
    "TimeGrid": false,
    "ForUnit": false,
    "UnitId": 0,
    "TimeBetween": "",
    "TimeBetweenEval": "n/a",
    "Facility": {
        "FacilityId": 687,
        "Name": "Creek Campground Lower Section (sites 36-115)",
        "Description": "Creek Campground Lower Section (sites 36-134)",
        "FacilityType": 2,
        "FacilityBehaviourType": 0,
        "FacilityMapSize": false,
        "FacilityImage": "California/Facilities/HSS_713_Creek_Campground_Lower_Section.jpg",
        "FacilityImageVBT": "https://cali-content.usedirect.com/Images/California/Facilities/HSS_713_Creek_Campground_Lower_Section.jpg",
        "DatesInSeason": 8,
        "DatesOutOfSeason": 0,
        "SeasonDates": {
            "2025-08-12T00:00:00": true,
            "2025-08-13T00:00:00": true,
            "2025-08-14T00:00:00": true,
            "2025-08-15T00:00:00": true,
            "2025-08-16T00:00:00": true,
            "2025-08-17T00:00:00": true,
            "2025-08-18T00:00:00": true,
            "2025-08-19T00:00:00": true
        },
        "TrafficStatuses": {},
        "UnitCount": 66,
        "AvailableUnitCount": 24,
        "SliceCount": 528,
        "AvailableSliceCount": 33,
        "Restrictions": {
            "FutureBookingStarts": "2025-08-12T00:00:00-07:00",
            "FutureBookingEnds": "2026-02-12T00:00:00-08:00",
            "MinimumStay": 1,
            "MaximumStay": 10,
            "IsRestrictionValid": true,
            "Time": "0001-01-01T00:00:00"
        },
        "Units": {
            "4645.1": {
                "UnitId": 45211,
                "Name": "Tent Campsite #C36",
                "ShortName": "C36",
                "RecentPopups": 0,
                "IsAda": false,
                "AllowWebBooking": true,
                "MapInfo": {
                    "UnitImage": "California/Units/tent",
                    "UnitImageVBT": "https://cali-content.usedirect.com/images/California/Units/tent.NotAvailable.png",
                    "ImageCoordinateX": 602,
                    "ImageCoordinateY": 106,
                    "ImageWidth": 35,
                    "ImageHeight": 35,
                    "FontSize": 8.0,
                    "Latitude": 0.0,
                    "Longitude": 0.0
                },
                "IsWebViewable": true,
                "IsFiltered": false,
                "UnitCategoryId": 1,
                "SleepingUnitIds": [
                    79,
                    83
                ],
                "UnitTypeGroupId": 5,
                "UnitTypeId": 4328,
                "UseType": 4,
                "VehicleLength": 35,
                "OrderBy": 36,
                "SliceCount": 8,
                "AvailableCount": 1,
                "IsFavourite": false,
                "Slices": {
                    "2025-08-12T00:00:00": {
                        "Date": "2025-08-12",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7297559,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-13T00:00:00": {
                        "Date": "2025-08-13",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7476753,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-14T00:00:00": {
                        "Date": "2025-08-14",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7476753,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-15T00:00:00": {
                        "Date": "2025-08-15",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7476043,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-16T00:00:00": {
                        "Date": "2025-08-16",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7471323,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-17T00:00:00": {
                        "Date": "2025-08-17",
                        "IsFree": true,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 0,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-18T00:00:00": {
                        "Date": "2025-08-18",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7325357,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-19T00:00:00": {
                        "Date": "2025-08-19",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7325357,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    }
                },
                "OrderByRaw": 4645,
                "StartTime": null,
                "EndTime": null
            },
            "4816.1": {
                "UnitId": 45213,
                "Name": "Campsite #C38",
                "ShortName": "C38",
                "RecentPopups": 0,
                "IsAda": false,
                "AllowWebBooking": true,
                "MapInfo": {
                    "UnitImage": "California/Units/tent",
                    "UnitImageVBT": "https://cali-content.usedirect.com/images/California/Units/tent.NotAvailable.png",
                    "ImageCoordinateX": 544,
                    "ImageCoordinateY": 106,
                    "ImageWidth": 35,
                    "ImageHeight": 35,
                    "FontSize": 8.0,
                    "Latitude": 0.0,
                    "Longitude": 0.0
                },
                "IsWebViewable": true,
                "IsFiltered": false,
                "UnitCategoryId": 1,
                "SleepingUnitIds": [
                    74,
                    75,
                    79,
                    83
                ],
                "UnitTypeGroupId": 1,
                "UnitTypeId": 4303,
                "UseType": 4,
                "VehicleLength": 35,
                "OrderBy": 38,
                "SliceCount": 8,
                "AvailableCount": 0,
                "IsFavourite": false,
                "Slices": {
                    "2025-08-12T00:00:00": {
                        "Date": "2025-08-12",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7411997,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-13T00:00:00": {
                        "Date": "2025-08-13",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7411997,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-14T00:00:00": {
                        "Date": "2025-08-14",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7454734,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-15T00:00:00": {
                        "Date": "2025-08-15",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7419685,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-16T00:00:00": {
                        "Date": "2025-08-16",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7419685,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-17T00:00:00": {
                        "Date": "2025-08-17",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7419685,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-18T00:00:00": {
                        "Date": "2025-08-18",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7467530,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    },
                    "2025-08-19T00:00:00": {
                        "Date": "2025-08-19",
                        "IsFree": false,
                        "IsBlocked": false,
                        "IsWalkin": false,
                        "ReservationId": 7457283,
                        "Lock": null,
                        "MinStay": 1,
                        "IsReservationDraw": false
                    }
                },
                "OrderByRaw": 4816,
                "StartTime": null,
                "EndTime": null
            },
            
        },
        "Latitude": 35.59713,
        "Longitude": -121.124702,
        "TimebaseMaxHours": 0,
        "TimebaseMinHours": 0,
        "TimebaseDuration": 0,
        "IsReservationDrawActive": true,
        "DrawBookingStartDate": "0001-01-01T00:00:00",
        "DrawBookingEndDate": "0001-01-01T00:00:00",
        "ReservationDrawDetail": null,
        "WalkinCounts": null
    }
}

Provider mapping used by this repo:
- Provider name: reservecalifornia
- CampgroundID: Use FacilityId from the grid/place responses (e.g., 687).
The provider fetch collapses requested dates into a single [min..max] range per FacilityId.