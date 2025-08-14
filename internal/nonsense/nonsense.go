package nonsense

import (
	"fmt"
	"math/rand"
	"time"
)

func RandomSillyGreeting(userID string) string {
	src := rand.NewSource(time.Now().UnixNano())
	r := rand.New(src)

	greetings := []string{
		"For fame to the eye of heaven is the blood of Cain. [The child of evil anointed at birth with the oils of hell.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) <@%s>, I give you the Red Horse of Schniff!",
		"There will be a day that is the end. [The collapse of time and all that stood within it.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) A day of nothing. But that day is not today. Today <@%s> burns bright with the desire to schniff!",
		"The humble consequence of carbon. The fleeting spray of life turned diamond by the sun. <@%s> will [curse and spit and sneer and shout their name at the heavens:](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) I AM THE SHINING ARC OF SCHNIFFING!",
		"In their last will and testament there is [a codicil memorializing their appreciation for string cheese](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) and all those who serve it. <@%s> is ranked No. 1 in the world of schniffing!",
		"<@%s> is a person so dedicated they were put in [prison in hell. Hell prison!](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) They survived by chewing seal bones and now they're here to schniff!",
		"<@%s> is [the eighth archangel. Gideon, the exalted.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) Six-feet nine inches tall. Seven feet from tip of wing to tip of wing. The kale-eating champion of the world, now the schniffing champion!",
		"Immediately following a record-setting performance, <@%s> dropped to one knee and [asked camping to marry them. Camping said 'yes.'](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) They are now the premier power couple in all of competitive schniffing!",
		"<@%s> has [greater muscle mass than two football players and a Canadian](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) but the key to their success is schniffing speed. When they schniff, their hands are a blur!",
		"<@%s> operates from a platform of power and has [zero respect for indecision.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) Impose your will on your schniff or have someone else impose their will on you!",
		"<@%s> lost the confidence of their co-workers when they mixed together all the food on their plate and said ['it's all going to the same place.'](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) Today they are universally acknowledged as the most efficient schniffist on the circuit!",
		"<@%s> is [the vortex at the center of the vortex.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) A child of the centuries selected for greatness by the finger of schniffing power!",
		"<@%s> has traveled this nation [from IHOP, Texas to Waffle House, Tennessee to Poke Bowl, Connecticut.](https://www.thebiglead.com/posts/ranking-george-shea-s-hot-dog-eating-introductions-during-the-performance-of-a-lifetime-01g757b67f0f) They have learned the common denominator is American schniffing exceptionalism!",
	}

	// Choose a random greeting template
	template := greetings[r.Intn(len(greetings))]

	// Substitute the user ID into the template
	greeting := fmt.Sprintf(template, userID)
	return greeting
}

func RandomSillyHeader() string {
	src := rand.NewSource(time.Now().UnixNano())
	r := rand.New(src)

	greetings := []string{
		"You've got schniff!",
		"Schnifffff!",
		"Another day, another schniff.",
		"Oh, what a schniff!",
		"Schniff, schniff, hooray!",
		"Look what the schniffer dragged in!",
		"Sch-sch-sch-sch-schniff!",
		"ka-schniff",
		"Schniffs ahoy",
		"I can't believe it's not schniff (it is)",
		"One flew over the schniffoo nest (into your inbox)",
	}

	// Choose a random greeting template
	return greetings[r.Intn(len(greetings))]

}

func RandomSillyBroadcast(userID string) string {
	src := rand.NewSource(time.Now().UnixNano())
	r := rand.New(src)

	greetings := []string{
		"I came, I saw, I schniffed <@%s> a campsite. ",
		"If schniffing were an olympic sport, <@%s> would be Steven Bradbury since I just found them a campsite.",
		"When you stare into the schniff, the schniff stares back. Is what <@%s> is saying right now because I found them a campsite.",
		"These messages are not generated by chatgpt. Neither is the campsite I just found for <@%s>.",
		"<@%s>'s the name, schniffing them a campsite is the game.",
		"<@%s> is thinking, why am I getting so many notifications? It's because I just successfully schniffed for them.",
		"Can <@%s> remember their recreation.gov login credentials? They'll need them to book the campsite I just found for them in time.",
		"That's one small schniff for <@%s>, one giant leap for schniffkind.",
		"The schniff will set <@%s> free. Free to book the campsite I just found for them. But not free, you have to pay.",
		"The only thing <@%s> has to schniff is schniff itself. And also the campsite I just found for them.",
		"80%% of success is showing up. The other 20%% is schniffing. <@%s> is now 100%% successful.",
		"Frankly, my dear, I don't give a schniff. But I did give a campsite to <@%s>.",
		"Hell is other schniffers. But heaven is a campsite I just found for <@%s>.",
		"I love the smell of schniff in the morning. It smells like <@%s>'s available campsite.",
		"If you want something done right, you have to do it yourself. Or you can just use schniffer and I'll do it for you, like I just did for <@%s>.",
		"I'm gonna schniff <@%s> a campsite they can't refuse. And actually I just did.",
		"Go ahead, make my schniff. I just found <@%s> a campsite.",
		"Tis better to have schniffed and lost than never to have schniffed at all. But <@%s> didn't lose, I just found them a campsite.",
		"What doesn't schniff you makes you stronger. <@%s> must be very weak since I just schniffed them a campsite.",
	}

	// Choose a random greeting template
	template := greetings[r.Intn(len(greetings))]
	return fmt.Sprintf(template, userID)

}
