// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ScratchArchiveDropsJournalProtection30 = [1] of {int}->
	run ScratchArchiveDropsJournalProtection3(child_ScratchArchiveDropsJournalProtection30)->
	child_ScratchArchiveDropsJournalProtection30?0->
stop_process:skip
}

proctype ScratchArchiveDropsJournalProtection3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousScratchArchiveDropsJournalProtection942 = [1] of {int}->
	chan child_AnonymousScratchArchiveDropsJournalProtection861 = [1] of {int}->
	chan child_AnonymousScratchArchiveDropsJournalProtection740 = [1] of {int}->
	Chandef deleted_ch->
	Chandef unreferenced_ch->
	Chandef survivor_ch->
	run sync_monitor(survivor_ch)->
	run sync_monitor(unreferenced_ch)->
	run sync_monitor(deleted_ch)->
	run AnonymousScratchArchiveDropsJournalProtection74(survivor_ch,unreferenced_ch,deleted_ch,child_AnonymousScratchArchiveDropsJournalProtection740)->
	run receiver(child_AnonymousScratchArchiveDropsJournalProtection740)->
	run AnonymousScratchArchiveDropsJournalProtection86(deleted_ch,survivor_ch,unreferenced_ch,child_AnonymousScratchArchiveDropsJournalProtection861)->
	run receiver(child_AnonymousScratchArchiveDropsJournalProtection861)->
	run AnonymousScratchArchiveDropsJournalProtection94(survivor_ch,unreferenced_ch,deleted_ch,child_AnonymousScratchArchiveDropsJournalProtection942)->
	run receiver(child_AnonymousScratchArchiveDropsJournalProtection942)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:10:2 */
	:: deleted_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:10:2 */->
	:: deleted_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:10:2 */ -> 
		deleted_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveDropsJournalProtection74(Chandef survivor_ch;Chandef unreferenced_ch;Chandef deleted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:7:12 */
	:: survivor_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:7:12 */
	:: survivor_ch.sync!false -> 
		survivor_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:7:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveDropsJournalProtection86(Chandef deleted_ch;Chandef survivor_ch;Chandef unreferenced_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:8:12 */
	:: unreferenced_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:8:12 */
	:: unreferenced_ch.sync!false -> 
		unreferenced_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:8:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousScratchArchiveDropsJournalProtection94(Chandef survivor_ch;Chandef unreferenced_ch;Chandef deleted_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:12 */
	:: survivor_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:12 */->
	:: survivor_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:12 */ -> 
		survivor_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:24 */
	:: deleted_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:24 */
	:: deleted_ch.sync!false -> 
		deleted_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/scratch_archive.go:9:24 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

